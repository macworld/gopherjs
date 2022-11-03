package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"time"

	"github.com/gopherjs/gopherjs/compiler/analysis"
	"github.com/gopherjs/gopherjs/compiler/astutil"
	"github.com/gopherjs/gopherjs/compiler/typesutil"
	"github.com/neelance/astrewrite"
	"golang.org/x/tools/go/gcexportdata"
)

// pkgContext maintains compiler context for a specific package.
type pkgContext struct {
	*analysis.Info
	additionalSelections map[*ast.SelectorExpr]selection

	typeNames    []*types.TypeName
	pkgVars      map[string]string
	objectNames  map[types.Object]string
	varPtrNames  map[*types.Var]string
	anonTypes    typesutil.AnonymousTypes
	escapingVars map[*types.Var]bool
	indentation  int
	dependencies map[types.Object]bool
	minify       bool
	fileSet      *token.FileSet
	errList      ErrorList
}

// genericCtx contains compiler context for a generic function or type.
//
// It is used to accumulate information about types and objects that depend on
// type parameters and must be constructed in a generic factory function.
type genericCtx struct {
	anonTypes typesutil.AnonymousTypes
}

func (p *pkgContext) SelectionOf(e *ast.SelectorExpr) (selection, bool) {
	if sel, ok := p.Selections[e]; ok {
		return sel, true
	}
	if sel, ok := p.additionalSelections[e]; ok {
		return sel, true
	}
	return nil, false
}

type selection interface {
	Kind() types.SelectionKind
	Recv() types.Type
	Index() []int
	Obj() types.Object
	Type() types.Type
}

type fakeSelection struct {
	kind  types.SelectionKind
	recv  types.Type
	index []int
	obj   types.Object
	typ   types.Type
}

func (sel *fakeSelection) Kind() types.SelectionKind { return sel.kind }
func (sel *fakeSelection) Recv() types.Type          { return sel.recv }
func (sel *fakeSelection) Index() []int              { return sel.index }
func (sel *fakeSelection) Obj() types.Object         { return sel.obj }
func (sel *fakeSelection) Type() types.Type          { return sel.typ }

// funcContext maintains compiler context for a specific function.
//
// An instance of this type roughly corresponds to a lexical scope for generated
// JavaScript code (as defined for `var` declarations).
type funcContext struct {
	*analysis.FuncInfo
	// Surrounding package context.
	pkgCtx *pkgContext
	// Surrounding generic function context. nil if non-generic code.
	genericCtx *genericCtx
	// Function context, surrounding this function definition. For package-level
	// functions or methods it is the package-level function context (even though
	// it technically doesn't correspond to a function). nil for the package-level
	// function context.
	parent *funcContext
	// Information about function signature types. nil for the package-level
	// function context.
	sigTypes *signatureTypes
	// All variable names available in the current function scope. The key is a Go
	// variable name and the value is the number of synonymous variable names
	// visible from this scope (e.g. due to shadowing). This number is used to
	// avoid conflicts when assigning JS variable names for Go variables.
	allVars map[string]int
	// Local JS variable names defined within this function context. This list
	// contains JS variable names assigned to Go variables, as well as other
	// auxiliary variables the compiler needs. It is used to generate `var`
	// declaration at the top of the function, as well as context save/restore.
	localVars []string
	// AST expressions representing function's named return values. nil if the
	// function has no return values or they are not named.
	resultNames []ast.Expr
	// Function's internal control flow graph used for generation of a "flattened"
	// version of the function when the function is blocking or uses goto.
	// TODO(nevkontakte): Describe the exact semantics of this map.
	flowDatas map[*types.Label]*flowData
	// Number of control flow blocks in a "flattened" function.
	caseCounter int
	// A mapping from Go labels statements (e.g. labelled loop) to the flow block
	// id corresponding to it.
	labelCases map[*types.Label]int
	// Generated code buffer for the current function.
	output []byte
	// Generated code that should be emitted at the end of the JS statement (?).
	delayedOutput []byte
	// Set to true if source position is available and should be emitted for the
	// source map.
	posAvailable bool
	// Current position in the Go source code.
	pos token.Pos
}

type flowData struct {
	postStmt  func()
	beginCase int
	endCase   int
}

type ImportContext struct {
	Packages map[string]*types.Package
	Import   func(string) (*Archive, error)
}

// packageImporter implements go/types.Importer interface.
type packageImporter struct {
	importContext *ImportContext
	importError   *error // A pointer to importError in Compile.
}

func (pi packageImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}

	a, err := pi.importContext.Import(path)
	if err != nil {
		if *pi.importError == nil {
			// If import failed, show first error of import only (https://github.com/gopherjs/gopherjs/issues/119).
			*pi.importError = err
		}
		return nil, err
	}

	return pi.importContext.Packages[a.ImportPath], nil
}

func Compile(importPath string, files []*ast.File, fileSet *token.FileSet, importContext *ImportContext, minify bool) (_ *Archive, err error) {
	defer func() {
		e := recover()
		if e == nil {
			return
		}
		if fe, ok := bailingOut(e); ok {
			// Orderly bailout, return whatever clues we already have.
			err = fe
			return
		}
		// Some other unexpected panic, catch the stack trace and return as an error.
		err = bailout(fmt.Errorf("unexpected compiler panic while building package %q: %v", importPath, e))
	}()

	// Files must be in the same order to get reproducible JS
	sort.Slice(files, func(i, j int) bool {
		return fileSet.File(files[i].Pos()).Name() > fileSet.File(files[j].Pos()).Name()
	})

	typesInfo := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:     make(map[ast.Node]*types.Scope),
		Instances:  make(map[*ast.Ident]types.Instance),
	}

	var errList ErrorList

	// Extract all go:linkname compiler directives from the package source.
	goLinknames := []GoLinkname{}
	for _, file := range files {
		found, err := parseGoLinknames(fileSet, importPath, file)
		if err != nil {
			if errs, ok := err.(ErrorList); ok {
				errList = append(errList, errs...)
			} else {
				errList = append(errList, err)
			}
		}
		goLinknames = append(goLinknames, found...)
	}

	var importError error
	var previousErr error
	config := &types.Config{
		Importer: packageImporter{
			importContext: importContext,
			importError:   &importError,
		},
		Sizes: sizes32,
		Error: func(err error) {
			if previousErr != nil && previousErr.Error() == err.Error() {
				return
			}
			errList = append(errList, err)
			previousErr = err
		},
	}
	typesPkg, err := config.Check(importPath, fileSet, files, typesInfo)
	if importError != nil {
		return nil, importError
	}
	if errList != nil {
		if len(errList) > 10 {
			pos := token.NoPos
			if last, ok := errList[9].(types.Error); ok {
				pos = last.Pos
			}
			errList = append(errList[:10], types.Error{Fset: fileSet, Pos: pos, Msg: "too many errors"})
		}
		return nil, errList
	}
	if err != nil {
		return nil, err
	}
	importContext.Packages[importPath] = typesPkg

	exportData := new(bytes.Buffer)
	if err := gcexportdata.Write(exportData, nil, typesPkg); err != nil {
		return nil, fmt.Errorf("failed to write export data: %v", err)
	}
	encodedFileSet := new(bytes.Buffer)
	if err := fileSet.Write(json.NewEncoder(encodedFileSet).Encode); err != nil {
		return nil, err
	}

	simplifiedFiles := make([]*ast.File, len(files))
	for i, file := range files {
		simplifiedFiles[i] = astrewrite.Simplify(file, typesInfo, false)
	}

	isBlocking := func(f *types.Func) bool {
		archive, err := importContext.Import(f.Pkg().Path())
		if err != nil {
			panic(err)
		}
		fullName := f.FullName()
		for _, d := range archive.Declarations {
			if string(d.FullName) == fullName {
				return d.Blocking
			}
		}
		panic(fullName)
	}
	pkgInfo := analysis.AnalyzePkg(simplifiedFiles, fileSet, typesInfo, typesPkg, isBlocking)
	funcCtx := &funcContext{
		FuncInfo: pkgInfo.InitFuncInfo,
		pkgCtx: &pkgContext{
			Info:                 pkgInfo,
			additionalSelections: make(map[*ast.SelectorExpr]selection),

			pkgVars:      make(map[string]string),
			objectNames:  make(map[types.Object]string),
			varPtrNames:  make(map[*types.Var]string),
			escapingVars: make(map[*types.Var]bool),
			indentation:  1,
			dependencies: make(map[types.Object]bool),
			minify:       minify,
			fileSet:      fileSet,
		},
		allVars:     make(map[string]int),
		flowDatas:   map[*types.Label]*flowData{nil: {}},
		caseCounter: 1,
		labelCases:  make(map[*types.Label]int),
	}
	for name := range reservedKeywords {
		funcCtx.allVars[name] = 1
	}

	// imports
	var importDecls []*Decl
	var importedPaths []string
	for _, importedPkg := range typesPkg.Imports() {
		if importedPkg == types.Unsafe {
			// Prior to Go 1.9, unsafe import was excluded by Imports() method,
			// but now we do it here to maintain previous behavior.
			continue
		}
		funcCtx.pkgCtx.pkgVars[importedPkg.Path()] = funcCtx.newVariable(importedPkg.Name(), varPackage)
		importedPaths = append(importedPaths, importedPkg.Path())
	}
	sort.Strings(importedPaths)
	for _, impPath := range importedPaths {
		id := funcCtx.newIdent(fmt.Sprintf(`%s.$init`, funcCtx.pkgCtx.pkgVars[impPath]), types.NewSignature(nil, nil, nil, false))
		call := &ast.CallExpr{Fun: id}
		funcCtx.Blocking[call] = true
		funcCtx.Flattened[call] = true
		importDecls = append(importDecls, &Decl{
			Vars:     []string{funcCtx.pkgCtx.pkgVars[impPath]},
			DeclCode: []byte(fmt.Sprintf("\t%s = $packages[\"%s\"];\n", funcCtx.pkgCtx.pkgVars[impPath], impPath)),
			InitCode: funcCtx.CatchOutput(1, func() { funcCtx.translateStmt(&ast.ExprStmt{X: call}, nil) }),
		})
	}

	var functions []*ast.FuncDecl
	var vars []*types.Var
	for _, file := range simplifiedFiles {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				sig := funcCtx.pkgCtx.Defs[d.Name].(*types.Func).Type().(*types.Signature)
				var recvType types.Type
				if sig.Recv() != nil {
					recvType = sig.Recv().Type()
					if ptr, isPtr := recvType.(*types.Pointer); isPtr {
						recvType = ptr.Elem()
					}
				}
				if sig.Recv() == nil {
					funcCtx.objectName(funcCtx.pkgCtx.Defs[d.Name].(*types.Func)) // register toplevel name
				}
				if !isBlank(d.Name) {
					functions = append(functions, d)
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						o := funcCtx.pkgCtx.Defs[spec.(*ast.TypeSpec).Name].(*types.TypeName)
						funcCtx.pkgCtx.typeNames = append(funcCtx.pkgCtx.typeNames, o)
						funcCtx.objectName(o) // register toplevel name
					}
				case token.VAR:
					for _, spec := range d.Specs {
						for _, name := range spec.(*ast.ValueSpec).Names {
							if !isBlank(name) {
								o := funcCtx.pkgCtx.Defs[name].(*types.Var)
								vars = append(vars, o)
								funcCtx.objectName(o) // register toplevel name
							}
						}
					}
				case token.CONST:
					// skip, constants are inlined
				}
			}
		}
	}

	collectDependencies := func(f func()) []string {
		funcCtx.pkgCtx.dependencies = make(map[types.Object]bool)
		f()
		var deps []string
		for o := range funcCtx.pkgCtx.dependencies {
			qualifiedName := o.Pkg().Path() + "." + o.Name()
			if f, ok := o.(*types.Func); ok && f.Type().(*types.Signature).Recv() != nil {
				deps = append(deps, qualifiedName+"~")
				continue
			}
			deps = append(deps, qualifiedName)
		}
		sort.Strings(deps)
		return deps
	}

	// variables
	var varDecls []*Decl
	varsWithInit := make(map[*types.Var]bool)
	for _, init := range funcCtx.pkgCtx.InitOrder {
		for _, o := range init.Lhs {
			varsWithInit[o] = true
		}
	}
	for _, o := range vars {
		var d Decl
		if !o.Exported() {
			d.Vars = []string{funcCtx.objectName(o)}
		}
		if funcCtx.pkgCtx.HasPointer[o] && !o.Exported() {
			d.Vars = append(d.Vars, funcCtx.varPtrName(o))
		}
		if _, ok := varsWithInit[o]; !ok {
			d.DceDeps = collectDependencies(func() {
				d.InitCode = []byte(fmt.Sprintf("\t\t%s = %s;\n", funcCtx.objectName(o), funcCtx.translateExpr(funcCtx.zeroValue(o.Type())).String()))
			})
		}
		d.DceObjectFilter = o.Name()
		varDecls = append(varDecls, &d)
	}
	for _, init := range funcCtx.pkgCtx.InitOrder {
		lhs := make([]ast.Expr, len(init.Lhs))
		for i, o := range init.Lhs {
			ident := ast.NewIdent(o.Name())
			ident.NamePos = o.Pos()
			funcCtx.pkgCtx.Defs[ident] = o
			lhs[i] = funcCtx.setType(ident, o.Type())
			varsWithInit[o] = true
		}
		var d Decl
		d.DceDeps = collectDependencies(func() {
			funcCtx.localVars = nil
			d.InitCode = funcCtx.CatchOutput(1, func() {
				funcCtx.translateStmt(&ast.AssignStmt{
					Lhs: lhs,
					Tok: token.DEFINE,
					Rhs: []ast.Expr{init.Rhs},
				}, nil)
			})
			d.Vars = append(d.Vars, funcCtx.localVars...)
		})
		if len(init.Lhs) == 1 {
			if !analysis.HasSideEffect(init.Rhs, funcCtx.pkgCtx.Info.Info) {
				d.DceObjectFilter = init.Lhs[0].Name()
			}
		}
		varDecls = append(varDecls, &d)
	}

	// functions
	var funcDecls []*Decl
	var mainFunc *types.Func
	for _, fun := range functions {
		o := funcCtx.pkgCtx.Defs[fun.Name].(*types.Func)

		funcInfo := funcCtx.pkgCtx.FuncDeclInfos[o]
		d := Decl{
			FullName: o.FullName(),
			Blocking: len(funcInfo.Blocking) != 0,
		}
		d.LinkingName = newSymName(o)
		if fun.Recv == nil {
			d.Vars = []string{funcCtx.objectName(o)}
			d.DceObjectFilter = o.Name()
			switch o.Name() {
			case "main":
				mainFunc = o
				d.DceObjectFilter = ""
			case "init":
				d.InitCode = funcCtx.CatchOutput(1, func() {
					id := funcCtx.newIdent("", types.NewSignature(nil, nil, nil, false))
					funcCtx.pkgCtx.Uses[id] = o
					call := &ast.CallExpr{Fun: id}
					if len(funcCtx.pkgCtx.FuncDeclInfos[o].Blocking) != 0 {
						funcCtx.Blocking[call] = true
					}
					funcCtx.translateStmt(&ast.ExprStmt{X: call}, nil)
				})
				d.DceObjectFilter = ""
			}
		} else {
			recvType := o.Type().(*types.Signature).Recv().Type()
			ptr, isPointer := recvType.(*types.Pointer)
			namedRecvType, _ := recvType.(*types.Named)
			if isPointer {
				namedRecvType = ptr.Elem().(*types.Named)
			}
			d.NamedRecvType = funcCtx.objectName(namedRecvType.Obj())
			d.DceObjectFilter = namedRecvType.Obj().Name()
			if !fun.Name.IsExported() {
				d.DceMethodFilter = o.Name() + "~"
			}
		}

		d.DceDeps = collectDependencies(func() {
			d.DeclCode = funcCtx.translateToplevelFunction(fun, funcInfo)
		})
		funcDecls = append(funcDecls, &d)
	}
	if typesPkg.Name() == "main" {
		if mainFunc == nil {
			return nil, fmt.Errorf("missing main function")
		}
		id := funcCtx.newIdent("", types.NewSignature(nil, nil, nil, false))
		funcCtx.pkgCtx.Uses[id] = mainFunc
		call := &ast.CallExpr{Fun: id}
		ifStmt := &ast.IfStmt{
			Cond: funcCtx.newIdent("$pkg === $mainPkg", types.Typ[types.Bool]),
			Body: &ast.BlockStmt{
				List: []ast.Stmt{
					&ast.ExprStmt{X: call},
					&ast.AssignStmt{
						Lhs: []ast.Expr{funcCtx.newIdent("$mainFinished", types.Typ[types.Bool])},
						Tok: token.ASSIGN,
						Rhs: []ast.Expr{funcCtx.newConst(types.Typ[types.Bool], constant.MakeBool(true))},
					},
				},
			},
		}
		if len(funcCtx.pkgCtx.FuncDeclInfos[mainFunc].Blocking) != 0 {
			funcCtx.Blocking[call] = true
			funcCtx.Flattened[ifStmt] = true
		}
		funcDecls = append(funcDecls, &Decl{
			InitCode: funcCtx.CatchOutput(1, func() {
				funcCtx.translateStmt(ifStmt, nil)
			}),
		})
	}

	// named types
	var typeDecls []*Decl
	for _, o := range funcCtx.pkgCtx.typeNames {
		if o.IsAlias() {
			continue
		}
		typeName := funcCtx.objectName(o)

		d := Decl{
			Vars:            []string{typeName},
			DceObjectFilter: o.Name(),
		}
		d.DceDeps = collectDependencies(func() {
			d.DeclCode = funcCtx.CatchOutput(0, func() {
				typeName := funcCtx.objectName(o)
				lhs := typeName
				if getVarLevel(o) == varPackage {
					lhs += " = $pkg." + encodeIdent(o.Name())
				}
				size := int64(0)
				constructor := "null"
				switch t := o.Type().Underlying().(type) {
				case *types.Struct:
					params := make([]string, t.NumFields())
					for i := 0; i < t.NumFields(); i++ {
						params[i] = fieldName(t, i) + "_"
					}
					constructor = fmt.Sprintf("function(%s) {\n\t\tthis.$val = this;\n\t\tif (arguments.length === 0) {\n", strings.Join(params, ", "))
					for i := 0; i < t.NumFields(); i++ {
						constructor += fmt.Sprintf("\t\t\tthis.%s = %s;\n", fieldName(t, i), funcCtx.translateExpr(funcCtx.zeroValue(t.Field(i).Type())).String())
					}
					constructor += "\t\t\treturn;\n\t\t}\n"
					for i := 0; i < t.NumFields(); i++ {
						constructor += fmt.Sprintf("\t\tthis.%[1]s = %[1]s_;\n", fieldName(t, i))
					}
					constructor += "\t}"
				case *types.Basic, *types.Array, *types.Slice, *types.Chan, *types.Signature, *types.Interface, *types.Pointer, *types.Map:
					size = sizes32.Sizeof(t)
				}
				if tPointer, ok := o.Type().Underlying().(*types.Pointer); ok {
					if _, ok := tPointer.Elem().Underlying().(*types.Array); ok {
						// Array pointers have non-default constructors to support wrapping
						// of the native objects.
						constructor = "$arrayPtrCtor()"
					}
				}
				funcCtx.Printf(`%s = $newType(%d, %s, "%s.%s", %t, "%s", %t, %s);`, lhs, size, typeKind(o.Type()), o.Pkg().Name(), o.Name(), o.Name() != "", o.Pkg().Path(), o.Exported(), constructor)
			})
			d.MethodListCode = funcCtx.CatchOutput(0, func() {
				named := o.Type().(*types.Named)
				if _, ok := named.Underlying().(*types.Interface); ok {
					return
				}
				var methods []string
				var ptrMethods []string
				for i := 0; i < named.NumMethods(); i++ {
					method := named.Method(i)
					name := method.Name()
					if reservedKeywords[name] {
						name += "$"
					}
					pkgPath := ""
					if !method.Exported() {
						pkgPath = method.Pkg().Path()
					}
					t := method.Type().(*types.Signature)
					entry := fmt.Sprintf(`{prop: "%s", name: %s, pkg: "%s", typ: $funcType(%s)}`, name, encodeString(method.Name()), pkgPath, funcCtx.initArgs(t))
					if _, isPtr := t.Recv().Type().(*types.Pointer); isPtr {
						ptrMethods = append(ptrMethods, entry)
						continue
					}
					methods = append(methods, entry)
				}
				if len(methods) > 0 {
					funcCtx.Printf("%s.methods = [%s];", funcCtx.typeName(named), strings.Join(methods, ", "))
				}
				if len(ptrMethods) > 0 {
					funcCtx.Printf("%s.methods = [%s];", funcCtx.typeName(types.NewPointer(named)), strings.Join(ptrMethods, ", "))
				}
			})
			switch t := o.Type().Underlying().(type) {
			case *types.Array, *types.Chan, *types.Interface, *types.Map, *types.Pointer, *types.Slice, *types.Signature, *types.Struct:
				d.TypeInitCode = funcCtx.CatchOutput(0, func() {
					funcCtx.Printf("%s.init(%s);", funcCtx.objectName(o), funcCtx.initArgs(t))
				})
			}
		})
		typeDecls = append(typeDecls, &d)
	}

	// anonymous types
	for _, t := range funcCtx.pkgCtx.anonTypes.Ordered() {
		d := Decl{
			Vars:            []string{t.Name()},
			DceObjectFilter: t.Name(),
		}
		d.DceDeps = collectDependencies(func() {
			d.DeclCode = []byte(fmt.Sprintf("\t%s = $%sType(%s);\n", t.Name(), strings.ToLower(typeKind(t.Type())[5:]), funcCtx.initArgs(t.Type())))
		})
		typeDecls = append(typeDecls, &d)
	}

	var allDecls []*Decl
	for _, d := range append(append(append(importDecls, typeDecls...), varDecls...), funcDecls...) {
		d.DeclCode = removeWhitespace(d.DeclCode, minify)
		d.MethodListCode = removeWhitespace(d.MethodListCode, minify)
		d.TypeInitCode = removeWhitespace(d.TypeInitCode, minify)
		d.InitCode = removeWhitespace(d.InitCode, minify)
		allDecls = append(allDecls, d)
	}

	if len(funcCtx.pkgCtx.errList) != 0 {
		return nil, funcCtx.pkgCtx.errList
	}

	return &Archive{
		ImportPath:   importPath,
		Name:         typesPkg.Name(),
		Imports:      importedPaths,
		ExportData:   exportData.Bytes(),
		Declarations: allDecls,
		FileSet:      encodedFileSet.Bytes(),
		Minified:     minify,
		GoLinknames:  goLinknames,
		BuildTime:    time.Now(),
	}, nil
}

func (fc *funcContext) initArgs(ty types.Type) string {
	switch t := ty.(type) {
	case *types.Array:
		return fmt.Sprintf("%s, %d", fc.typeName(t.Elem()), t.Len())
	case *types.Chan:
		return fmt.Sprintf("%s, %t, %t", fc.typeName(t.Elem()), t.Dir()&types.SendOnly != 0, t.Dir()&types.RecvOnly != 0)
	case *types.Interface:
		methods := make([]string, t.NumMethods())
		for i := range methods {
			method := t.Method(i)
			pkgPath := ""
			if !method.Exported() {
				pkgPath = method.Pkg().Path()
			}
			methods[i] = fmt.Sprintf(`{prop: "%s", name: "%s", pkg: "%s", typ: $funcType(%s)}`, method.Name(), method.Name(), pkgPath, fc.initArgs(method.Type()))
		}
		return fmt.Sprintf("[%s]", strings.Join(methods, ", "))
	case *types.Map:
		return fmt.Sprintf("%s, %s", fc.typeName(t.Key()), fc.typeName(t.Elem()))
	case *types.Pointer:
		return fmt.Sprintf("%s", fc.typeName(t.Elem()))
	case *types.Slice:
		return fmt.Sprintf("%s", fc.typeName(t.Elem()))
	case *types.Signature:
		params := make([]string, t.Params().Len())
		for i := range params {
			params[i] = fc.typeName(t.Params().At(i).Type())
		}
		results := make([]string, t.Results().Len())
		for i := range results {
			results[i] = fc.typeName(t.Results().At(i).Type())
		}
		return fmt.Sprintf("[%s], [%s], %t", strings.Join(params, ", "), strings.Join(results, ", "), t.Variadic())
	case *types.Struct:
		pkgPath := ""
		fields := make([]string, t.NumFields())
		for i := range fields {
			field := t.Field(i)
			if !field.Exported() {
				pkgPath = field.Pkg().Path()
			}
			fields[i] = fmt.Sprintf(`{prop: "%s", name: %s, embedded: %t, exported: %t, typ: %s, tag: %s}`, fieldName(t, i), encodeString(field.Name()), field.Anonymous(), field.Exported(), fc.typeName(field.Type()), encodeString(t.Tag(i)))
		}
		return fmt.Sprintf(`"%s", [%s]`, pkgPath, strings.Join(fields, ", "))
	default:
		err := bailout(fmt.Errorf("%v has unexpected type %T", ty, ty))
		panic(err)
	}
}

func (fc *funcContext) translateToplevelFunction(fun *ast.FuncDecl, info *analysis.FuncInfo) []byte {
	o := fc.pkgCtx.Defs[fun.Name].(*types.Func)
	sig := o.Type().(*types.Signature)
	var recv *ast.Ident
	if fun.Recv != nil && fun.Recv.List[0].Names != nil {
		recv = fun.Recv.List[0].Names[0]
	}

	var joinedParams string
	primaryFunction := func(funcRef string) []byte {
		if fun.Body == nil {
			return []byte(fmt.Sprintf("\t%s = function() {\n\t\t$throwRuntimeError(\"native function not implemented: %s\");\n\t};\n", funcRef, o.FullName()))
		}

		params, fun := translateFunction(fun.Type, recv, fun.Body, fc, sig, info, funcRef)
		joinedParams = strings.Join(params, ", ")
		return []byte(fmt.Sprintf("\t%s = %s;\n", funcRef, fun))
	}

	code := bytes.NewBuffer(nil)

	if fun.Recv == nil {
		funcRef := fc.objectName(o)
		code.Write(primaryFunction(funcRef))
		if fun.Name.IsExported() {
			fmt.Fprintf(code, "\t$pkg.%s = %s;\n", encodeIdent(fun.Name.Name), funcRef)
		}
		return code.Bytes()
	}

	recvType := sig.Recv().Type()
	ptr, isPointer := recvType.(*types.Pointer)
	namedRecvType, _ := recvType.(*types.Named)
	if isPointer {
		namedRecvType = ptr.Elem().(*types.Named)
	}
	typeName := fc.objectName(namedRecvType.Obj())
	funName := fun.Name.Name
	if reservedKeywords[funName] {
		funName += "$"
	}

	if _, isStruct := namedRecvType.Underlying().(*types.Struct); isStruct {
		code.Write(primaryFunction(typeName + ".ptr.prototype." + funName))
		fmt.Fprintf(code, "\t%s.prototype.%s = function(%s) { return this.$val.%s(%s); };\n", typeName, funName, joinedParams, funName, joinedParams)
		return code.Bytes()
	}

	if isPointer {
		if _, isArray := ptr.Elem().Underlying().(*types.Array); isArray {
			code.Write(primaryFunction(typeName + ".prototype." + funName))
			fmt.Fprintf(code, "\t$ptrType(%s).prototype.%s = function(%s) { return (new %s(this.$get())).%s(%s); };\n", typeName, funName, joinedParams, typeName, funName, joinedParams)
			return code.Bytes()
		}
		return primaryFunction(fmt.Sprintf("$ptrType(%s).prototype.%s", typeName, funName))
	}

	value := "this.$get()"
	if typesutil.IsGeneric(recvType) {
		value = fmt.Sprintf("%s.wrap(%s)", typeName, value)
	} else if isWrapped(recvType) {
		value = fmt.Sprintf("new %s(%s)", typeName, value)
	}
	code.Write(primaryFunction(typeName + ".prototype." + funName))
	fmt.Fprintf(code, "\t$ptrType(%s).prototype.%s = function(%s) { return %s.%s(%s); };\n", typeName, funName, joinedParams, value, funName, joinedParams)
	return code.Bytes()
}

func translateFunction(typ *ast.FuncType, recv *ast.Ident, body *ast.BlockStmt, outerContext *funcContext, sig *types.Signature, info *analysis.FuncInfo, funcRef string) ([]string, string) {
	if info == nil {
		panic("nil info")
	}

	c := &funcContext{
		FuncInfo:    info,
		pkgCtx:      outerContext.pkgCtx,
		genericCtx:  outerContext.genericCtx,
		parent:      outerContext,
		sigTypes:    &signatureTypes{Sig: sig},
		allVars:     make(map[string]int, len(outerContext.allVars)),
		localVars:   []string{},
		flowDatas:   map[*types.Label]*flowData{nil: {}},
		caseCounter: 1,
		labelCases:  make(map[*types.Label]int),
	}
	for k, v := range outerContext.allVars {
		c.allVars[k] = v
	}
	if c.sigTypes.IsGeneric() {
		c.genericCtx = &genericCtx{}
		funcRef = c.newVariable(funcRef, varGenericFactory)
	}
	prevEV := c.pkgCtx.escapingVars

	var params []string
	for _, param := range typ.Params.List {
		if len(param.Names) == 0 {
			params = append(params, c.newLocalVariable("param"))
			continue
		}
		for _, ident := range param.Names {
			if isBlank(ident) {
				params = append(params, c.newLocalVariable("param"))
				continue
			}
			params = append(params, c.objectName(c.pkgCtx.Defs[ident]))
		}
	}

	bodyOutput := string(c.CatchOutput(c.bodyIndent(), func() {
		if len(c.Blocking) != 0 {
			c.pkgCtx.Scopes[body] = c.pkgCtx.Scopes[typ]
			c.handleEscapingVars(body)
		}

		if c.sigTypes != nil && c.sigTypes.HasNamedResults() {
			c.resultNames = make([]ast.Expr, c.sigTypes.Sig.Results().Len())
			for i := 0; i < c.sigTypes.Sig.Results().Len(); i++ {
				result := c.sigTypes.Sig.Results().At(i)
				c.Printf("%s = %s;", c.objectName(result), c.translateExpr(c.zeroValue(result.Type())).String())
				id := ast.NewIdent("")
				c.pkgCtx.Uses[id] = result
				c.resultNames[i] = c.setType(id, result.Type())
			}
		}

		if recv != nil && !isBlank(recv) {
			this := "this"
			if isWrapped(c.pkgCtx.TypeOf(recv)) {
				this = "this.$val" // Unwrap receiver value.
			}
			c.Printf("%s = %s;", c.translateExpr(recv), this)
		}

		c.translateStmtList(body.List)
		if len(c.Flattened) != 0 && !astutil.EndsWithReturn(body.List) {
			c.translateStmt(&ast.ReturnStmt{}, nil)
		}
	}))

	sort.Strings(c.localVars)

	var prefix, suffix, functionName string

	if len(c.Flattened) != 0 {
		c.localVars = append(c.localVars, "$s")
		prefix = prefix + " $s = $s || 0;"
	}

	if c.HasDefer {
		c.localVars = append(c.localVars, "$deferred")
		suffix = " }" + suffix
		if len(c.Blocking) != 0 {
			suffix = " }" + suffix
		}
	}

	localVarDefs := "" // Function-local var declaration at the top.

	if len(c.Blocking) != 0 {
		if funcRef == "" {
			funcRef = "$b"
			functionName = " $b"
		}

		localVars := append([]string{}, c.localVars...)
		// There are several special variables involved in handling blocking functions:
		// $r is sometimes used as a temporary variable to store blocking call result.
		// $c indicates that a function is being resumed after a blocking call when set to true.
		// $f is an object used to save and restore function context for blocking calls.
		localVars = append(localVars, "$r")
		// If a blocking function is being resumed, initialize local variables from the saved context.
		localVarDefs = fmt.Sprintf("var {%s, $c} = $restore(this, {%s});\n", strings.Join(localVars, ", "), strings.Join(params, ", "))
		// If the function gets blocked, save local variables for future.
		saveContext := fmt.Sprintf("var $f = {$blk: "+funcRef+", $c: true, $r, %s};", strings.Join(c.localVars, ", "))

		suffix = " " + saveContext + "return $f;" + suffix
	} else if len(c.localVars) > 0 {
		// Non-blocking functions simply declare local variables with no need for restore support.
		localVarDefs = fmt.Sprintf("var %s;\n", strings.Join(c.localVars, ", "))
	}

	if c.HasDefer {
		prefix = prefix + " var $err = null; try {"
		deferSuffix := " } catch(err) { $err = err;"
		if len(c.Blocking) != 0 {
			deferSuffix += " $s = -1;"
		}
		if c.resultNames == nil && c.sigTypes.HasResults() {
			deferSuffix += fmt.Sprintf(" return%s;", c.translateResults(nil))
		}
		deferSuffix += " } finally { $callDeferred($deferred, $err);"
		if c.resultNames != nil {
			deferSuffix += fmt.Sprintf(" if (!$curGoroutine.asleep) { return %s; }", c.translateResults(c.resultNames))
		}
		if len(c.Blocking) != 0 {
			deferSuffix += " if($curGoroutine.asleep) {"
		}
		suffix = deferSuffix + suffix
	}

	if len(c.Flattened) != 0 {
		prefix = prefix + " s: while (true) { switch ($s) { case 0:"
		suffix = " } return; }" + suffix
	}

	if c.HasDefer {
		prefix = prefix + " $deferred = []; $curGoroutine.deferStack.push($deferred);"
	}

	if prefix != "" {
		bodyOutput = c.Indentation(c.bodyIndent()) + "/* */" + prefix + "\n" + bodyOutput
	}
	if suffix != "" {
		bodyOutput = bodyOutput + c.Indentation(c.bodyIndent()) + "/* */" + suffix + "\n"
	}
	if localVarDefs != "" {
		bodyOutput = c.Indentation(c.bodyIndent()) + localVarDefs + bodyOutput
	}

	c.pkgCtx.escapingVars = prevEV

	if !c.sigTypes.IsGeneric() {
		return params, fmt.Sprintf("function%s(%s) {\n%s%s}", functionName, strings.Join(params, ", "), bodyOutput, c.Indentation(0))
	}

	// Generic functions are generated as factories to allow passing type parameters
	// from the call site.
	// TODO(nevkontakte): Cache function instances for a given combination of type
	// parameters.
	typeParams := []string{}
	for i := 0; i < c.sigTypes.Sig.TypeParams().Len(); i++ {
		typeParam := c.sigTypes.Sig.TypeParams().At(i)
		typeParams = append(typeParams, c.typeName(typeParam))
	}

	// anonymous types
	typesInit := strings.Builder{}
	for _, t := range c.genericCtx.anonTypes.Ordered() {
		fmt.Fprintf(&typesInit, "%svar %s = $%sType(%s);\n", c.Indentation(1), t.Name(), strings.ToLower(typeKind(t.Type())[5:]), c.initArgs(t.Type()))
	}

	code := &strings.Builder{}
	fmt.Fprintf(code, "function%s(%s){\n", functionName, strings.Join(typeParams, ", "))
	fmt.Fprintf(code, "%s", typesInit.String())
	fmt.Fprintf(code, "%sconst %s = function(%s) {\n", c.Indentation(1), funcRef, strings.Join(params, ", "))
	fmt.Fprintf(code, "%s", bodyOutput)
	fmt.Fprintf(code, "%s};\n", c.Indentation(1))
	fmt.Fprintf(code, "%sreturn %s;\n", c.Indentation(1), funcRef)
	fmt.Fprintf(code, "%s}", c.Indentation(0))
	return params, code.String()
}

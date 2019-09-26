package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/moznion/gowrtr/generator"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/typeutil"
)

type typeInfo struct {
	name string
}

func (ti typeInfo) String() string {
	return ti.name
}

func (ti typeInfo) VariadicString() string {
	return "..." + ti.name[2:]
}

type funcInfo struct {
	variadic bool
	orig typeInfo
	params, results []typeInfo
}

func (fi funcInfo) funcParameters() []*generator.FuncParameter {
	ret := make([]*generator.FuncParameter, len(fi.params))
	for i, p := range fi.params {
		typ := p.String()
		if fi.variadic && i == len(fi.params) - 1 {
			typ = p.VariadicString()
		}
		ret[i] = generator.NewFuncParameter("p" + strconv.Itoa(i), typ)
	}
	return ret
}

func (fi funcInfo) funcReturnTypes() []*generator.FuncReturnType {
	ret := make([]*generator.FuncReturnType, len(fi.results))
	for i, r := range fi.results {
		ret[i] = generator.NewFuncReturnType(r.String(), "r" + strconv.Itoa(i))
	}
	return ret
}

func (fi funcInfo) simpleSignature() string {
	b := new(strings.Builder)
	fmt.Fprint(b, "func(")
	for i, p := range fi.params {
		if fi.variadic && i == len(fi.params) - 1 {
			fmt.Fprint(b, p.VariadicString())
		} else {
			fmt.Fprint(b, p.String())
		}
		if i < len(fi.params) - 1 {
			fmt.Fprint(b, ", ")
		}
	}
	fmt.Fprint(b, ") (")
	for i, r := range fi.results {
		fmt.Fprint(b, r.String())
		if i < len(fi.results) - 1 {
			fmt.Fprint(b, ", ")
		}
	}
	fmt.Fprint(b, ")")
	return b.String()
}

type infoResolver struct {
	qualifier types.Qualifier
}

func newInfoResolver(pathToName map[string]string, pkg *types.Package) infoResolver {
	return infoResolver{types.Qualifier(func(other *types.Package) string {
		if pkg == other || pathToName[other.Path()] == "." {
			return ""
		}
		return pathToName[other.Path()]
	})}
}

func (ir infoResolver) resolveFuncInfo(typ types.Type) funcInfo {
	sig, ok := typ.Underlying().(*types.Signature)
	if !ok {
		log.Fatalln("Unexpected non-function")
	}
	fmt.Println("Signature:", sig)
	params := sig.Params()
	results := sig.Results()
	fi := funcInfo{
		variadic: sig.Variadic(),
		orig: typeInfo{name: types.TypeString(typ, ir.qualifier)},
		params: make([]typeInfo, params.Len()),
		results: make([]typeInfo, results.Len()),
	}
	for i := 0; i < params.Len(); i++ {
		p := params.At(i)
		fi.params[i] = typeInfo{
			name: types.TypeString(p.Type(), ir.qualifier),
		}
	}
	for i := 0; i < results.Len(); i++ {
		r := results.At(i)
		fi.results[i] = typeInfo{
			name: types.TypeString(r.Type(), ir.qualifier),
		}
	}
	return fi
}

func wrapperName(hash uint32) string {
	return "_isuprofWrapper" + strconv.Itoa(int(hash))
}

func main() {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "example.go", src, parser.Mode(0))
	if err != nil {
		log.Fatal("failed to parse file:", err)
	}

	conf := types.Config{Importer: importer.Default()}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
		Implicits: make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes: make(map[ast.Node]*types.Scope),
	}
	pkg, err := conf.Check("path/to/pkg", fset, []*ast.File{f}, info)
	if err != nil {
		log.Fatal("failed to type-check:", err)
	}
	_ = pkg

	pathToName := make(map[string]string)

	for _, v := range info.Defs {
		if pkgName, ok := v.(*types.PkgName); ok {
			pathToName[pkgName.Imported().Path()] = pkgName.Name()
		}
	}

	for _, v := range info.Implicits {
		if pkgName, ok := v.(*types.PkgName); ok {
			pathToName[pkgName.Imported().Path()] = pkgName.Name()
		}
	}

	resolver := newInfoResolver(pathToName, pkg)

	wrtr := generator.NewRoot(
		generator.NewComment(" +build profiling"),
		generator.NewNewline(),
		generator.NewPackage(pkg.Name()),
		generator.NewNewline(),
	)

	hasher := typeutil.MakeHasher()
	signatures := make(map[uint32][]string)
	funcDecls := make(map[uint32]string)

	n := astutil.Apply(f, func(cr *astutil.Cursor) bool {
		if ce, ok := cr.Node().(*ast.CallExpr); ok {
			tav, ok := info.Types[ce.Fun]
			if !ok || !tav.IsValue() {
				// type not found or expr is a type conversion or a builtin call
				return true
			}
			hash := hasher.Hash(tav.Type)
			if _, ok := signatures[hash]; !ok {
				fi := resolver.resolveFuncInfo(tav.Type)
				newFunc := generator.NewFunc(
					nil,
					generator.NewFuncSignature(
						wrapperName(hash),
					).AddParameters(
						append(
							[]*generator.FuncParameter{generator.NewFuncParameter("a", fi.orig.String())},
							fi.funcParameters()...
						)...
					).AddReturnTypeStatements(fi.funcReturnTypes()...),
					generator.NewRawStatement("pc := reflect.ValueOf(a).Pointer()"),
					generator.NewRawStatement("name := runtime.FuncForPC(pc).Name()"),
					generator.NewRawStatementf("p := _isuprofStartProfiling(name%s)", func() string {
						if len(fi.params) == 0 { return "" }
						b := new(strings.Builder)
						for i := 0; i < len(fi.params); i++ {
							fmt.Fprint(b, ", p" + strconv.Itoa(i))
						}
						if fi.variadic {
							fmt.Fprint(b, "...")
						}
						return b.String()
					}()),
					generator.NewRawStatementf("%sa(%s)", func() string {
						if len(fi.results) == 0 { return "" }
						b := new(strings.Builder)
						for i := 0; i < len(fi.results); i++ {
							if i > 0 {
								fmt.Fprint(b, ", ")
							}
							fmt.Fprint(b, "r" + strconv.Itoa(i))
						}
						fmt.Fprint(b, " = ")
						return b.String()
					}(), func() string {
						if len(fi.results) == 0 { return "" }
						b := new(strings.Builder)
						for i := 0; i < len(fi.params); i++ {
							if i > 0 {
								fmt.Fprint(b, ", ")
							}
							fmt.Fprint(b, "p" + strconv.Itoa(i))
						}
						if fi.variadic {
							fmt.Fprint(b, "...")
						}
						return b.String()
					}()),
					generator.NewRawStatementf("p.stopProfiling(%s)", func() string {
						if len(fi.results) == 0 { return "" }
						b := new(strings.Builder)
						for i := 0; i < len(fi.results); i++ {
							if i > 0 {
								fmt.Fprint(b, ", ")
							}
							fmt.Fprint(b, "r" + strconv.Itoa(i))
						}
						return b.String()
					}()),
				)
				generated, err := newFunc.Generate(0)
				if err != nil {
					log.Println("failed to generate a new func:", err)
					return true
				}
				funcDecls[hash] = generated
			}
			signatures[hash] = append(signatures[hash], types.ExprString((ce)))
			cr.Replace(&ast.CallExpr{
				Fun: &ast.Ident{
					Name: wrapperName(hash),
					Obj: ast.NewObj(ast.Fun, wrapperName(hash)),
				},
				Args: append([]ast.Expr{ce.Fun}, ce.Args...),
			})
		}
		return true
	}, nil)
	for path, name := range pathToName {
		wrtr = wrtr.AddStatements(
			generator.NewRawStatementf("import %s %s", name, strconv.Quote(path)),
		)
	}
	for _, v := range funcDecls {
		wrtr = wrtr.AddStatements(generator.NewRawStatement(v))
	}
	if err := format.Node(os.Stdout, token.NewFileSet(), n); err != nil {
		log.Fatal(err)
	}
	wrtr = wrtr.Gofmt("-s").Goimports()
	generated, err := wrtr.Generate(0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(generated)
}

var src = `
// +build linux

package p

import (
	"fmt"
	_ "math/rand"
	mmm "github.com/gorilla/mux"
	. "github.com/moznion/gowrtr/generator"
	"github.com/zenazn/goji/web"
)

func unko(a, b int, c float32) float32 {
	return c
}

func either(b bool) func() {
	if b {
		ret := func() {
			fmt.Println("true")
		}
		return ret
	} else {
		return func() {
			fmt.Println("false")
		}
	}
}

type xx struct {
	n int
}

func NewXx() xx {
	return xx{3}
}

func (x xx) N(a, b bool) int {
	return x.n
}

type xy = xx

func N(a bool, b bool) int {
	return 1
}

type M func(a, b bool) int

type L M

func main() {
	n, _ := fmt.Println(34, "hoge")
	unko(n, int(3), 3)
	either(true)()
	e := either(false)
	e()
	NewXx().N(true, true)
	N(true, false)
	xy(NewXx()).N(false, false)
	L((func(bool, bool)int)(M(N)))(true, false)
	lmn := L(M(N))
	lmn(false, true)
	func(a int) *mmm.Router { return nil }(1)
	_ = make([]int, 0)
	func() (*web.Mux, *Root) { return nil, nil }()
}
`
/*
func _isuprof_wrapper_x(a func(bool, bool) int, p1 bool, p2 bool) (r1 int) {
	p := _isuprof_start_profiling(runtime.FuncForPC(reflect.ValueOf(a).Pointer()).Name(), p1, p2)
	r1 = a(p1, p2)
	p.finishProfiling(r1)
}
*/
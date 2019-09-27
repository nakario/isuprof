package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
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
		if name, ok := pathToName[other.Path()]; ok {
			return name
		}
		return other.Name()
	})}
}

func (ir infoResolver) resolveFuncInfo(typ types.Type) funcInfo {
	sig, ok := typ.Underlying().(*types.Signature)
	if !ok {
		log.Fatalln("Unexpected non-function")
	}
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

func (ir infoResolver) generateWrapper(name string, t types.Type) string {
	fi := ir.resolveFuncInfo(t)
	newFunc := generator.NewFunc(
		nil,
		generator.NewFuncSignature(
			name,
		).AddParameters(
			append(
				[]*generator.FuncParameter{generator.NewFuncParameter("a", fi.orig.String())},
				fi.funcParameters()...
			)...
		).AddReturnTypeStatements(fi.funcReturnTypes()...),
		generator.NewRawStatement("pc := reflect.ValueOf(a).Pointer()"),
		generator.NewRawStatement("name := runtime.FuncForPC(pc).Name()"),
		generator.NewRawStatementf("ps := []interface{}{%s}", func() string {
			length := len(fi.params)
			if fi.variadic { length-- }
			if length == 0 { return "" }
			b := new(strings.Builder)
			for i := 0; i < length; i++ {
				if i > 0 {
					fmt.Fprint(b, ", ")
				}
				fmt.Fprint(b, "p" + strconv.Itoa(i))
			}
			return b.String()
		}()),
		generator.NewRawStatement(func() string {
			if !fi.variadic { return "" }
			return "for i := 0; i < len(p" + strconv.Itoa(len(fi.params) - 1) + "); i++ { ps = append(ps, p" + strconv.Itoa(len(fi.params) - 1) + "[i]) }"
		}()),
		generator.NewRawStatement("p := _isuprofStartProfiling(name, ps...)"),
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
			if len(fi.params) == 0 { return "" }
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
		generator.NewRawStatement("return"),
	)
	generated, err := newFunc.Generate(0)
	if err != nil {
		log.Fatal("failed to generate a new func:", err)
	}
	return generated
}

func wrapperName(hash uint32) string {
	return "_isuprofWrapper" + strconv.Itoa(int(hash))
}

type Hasher struct {
	hasher typeutil.Hasher
	forward map[types.Type]uint32
	backward map[uint32]types.Type
}

func newHasher() Hasher {
	return Hasher{
		hasher: typeutil.MakeHasher(),
		forward: make(map[types.Type]uint32),
		backward: make(map[uint32]types.Type),
	}
}

func (h Hasher) Hash(t types.Type) uint32 {
	hash, ok := h.forward[t]
	if !ok {
		hash = h.hasher.Hash(t)
		t2, exists := h.backward[hash]
		i := 0
		for exists {
			if types.Identical(t, t2) {
				h.forward[t] = hash
				return hash
			}
			i++
			if i > 10000 {
				panic("too many hash collisions!")
			}
			hash++
			t2, exists = h.backward[hash]
		}
		h.forward[t] = hash
		h.backward[hash] = t
	}
	return hash
}

func main() {
	if len(os.Args) != 2 {
		exe, err := os.Executable()
		if err != nil {
			log.Fatal("failed to get executable name:", err)
		}
		fmt.Println("Usage:", exe, "path/to/dir")
		return
	}
	dir, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Fatal("failed to get absolute path of", os.Args[0], ":", err)
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.Mode(0))
	if err != nil {
		log.Fatalf("failed to parse dir %s: %s", dir, err.Error())
	}
	files := make([]*ast.File, 0)
	for _, f := range pkgs["main"].Files {
		files = append(files, f)
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
	pkg, err := conf.Check(dir, fset, files, info)
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

	hasher := newHasher()
	funcDecls := make(map[uint32]string)

	for fname, f := range pkgs["main"].Files {
		n := astutil.Apply(f, func(cr *astutil.Cursor) bool {
			if ce, ok := cr.Node().(*ast.CallExpr); ok {
				tav, ok := info.Types[ce.Fun]
				if !ok || !tav.IsValue() {
					// type not found or expr is a type conversion or a builtin call
					return true
				}
				hash := hasher.Hash(tav.Type)
				if _, ok := funcDecls[hash]; !ok {
					generated := resolver.generateWrapper(wrapperName(hash), tav.Type)
					funcDecls[hash] = generated
				}
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
		buf := new(bytes.Buffer)
		if err := format.Node(buf, token.NewFileSet(), n); err != nil {
			log.Fatal(err)
		}
		wd, _ := os.Getwd()
		if err := ioutil.WriteFile(filepath.Join(wd, "build", filepath.Base(fname)), buf.Bytes(), 0666); err != nil {
			log.Fatal(err)
		}
	}
	for path, name := range pathToName {
		wrtr = wrtr.AddStatements(
			generator.NewRawStatementf("import %s %s", name, strconv.Quote(path)),
		)
	}
	for _, v := range funcDecls {
		wrtr = wrtr.AddStatements(generator.NewRawStatement(v))
	}
	wrtr = wrtr.AddStatements(
		generator.NewRawStatement(`
		type _isuprofProfiler struct {
			funcName string
			startedAt time.Time
			params, results []interface{}
		}
		
		func _isuprofStartProfiling(name string, params ...interface{}) *_isuprofProfiler {
			return &_isuprofProfiler{
				funcName: name,
				startedAt: time.Now(),
				params: params,
			}
		}
		
		func (p _isuprofProfiler) stopProfiling(results ...interface{}) {
			elapsed := time.Now().Sub(p.startedAt)
			log.Printf("{\"elapsed\": %d, \"name\": \"%s\"}", elapsed, p.funcName)
		}`),
	)
	wrtr = wrtr.Goimports().Gofmt("-s")
	generated, err := wrtr.Generate(0)
	if err != nil {
		log.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := ioutil.WriteFile(filepath.Join(wd, "build", "isuprof_generated.go"), []byte(generated), 0666); err != nil {
		log.Fatal("failed to write generated.go:", err)
	}
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

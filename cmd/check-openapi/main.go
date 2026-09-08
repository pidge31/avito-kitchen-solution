// Команда check-openapi проверяет спецификацию и сопоставляет её с зарегистрированными маршрутами
package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	spec, err := os.ReadFile("openapi/openapi.yaml")
	if err != nil {
		return err
	}
	source, err := os.ReadFile("internal/httpapi/api.go")
	if err != nil {
		return err
	}
	count, err := check(spec, source)
	if err != nil {
		return err
	}
	fmt.Printf("OpenAPI is valid; all %d routes are covered.\n", count)
	return nil
}

func check(spec, source []byte) (int, error) {
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return 0, fmt.Errorf("load OpenAPI: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return 0, fmt.Errorf("invalid OpenAPI: %w", err)
	}
	if doc.Components == nil {
		return 0, fmt.Errorf("OpenAPI must declare security schemes")
	}
	partner := doc.Components.SecuritySchemes["PartnerKey"]
	gateway := doc.Components.SecuritySchemes["GatewayKey"]
	if partner == nil || partner.Value == nil || partner.Value.Type != "http" || partner.Value.Scheme != "bearer" {
		return 0, fmt.Errorf("PartnerKey must describe HTTP bearer authentication")
	}
	if gateway == nil || gateway.Value == nil || gateway.Value.Type != "apiKey" || gateway.Value.In != "header" || gateway.Value.Name != "X-Gateway-Key" {
		return 0, fmt.Errorf("GatewayKey must describe the X-Gateway-Key header")
	}
	actual, err := registeredRoutes(source)
	if err != nil {
		return 0, err
	}
	declared := make(map[string]bool)
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			route := method + " " + path
			declared[route] = true
			securityName := ""
			if strings.HasPrefix(path, "/v1/partner/") {
				securityName = "PartnerKey"
			} else if path == "/v1/orders" || strings.HasPrefix(path, "/v1/orders/") {
				securityName = "GatewayKey"
			}
			if securityName != "" {
				security := operation.Security
				if security == nil || len(*security) != 1 || len((*security)[0]) != 1 {
					return 0, fmt.Errorf("%s: security must be [{%s: []}]", route, securityName)
				}
				scopes, ok := (*security)[0][securityName]
				if !ok || len(scopes) != 0 {
					return 0, fmt.Errorf("%s: security must be [{%s: []}]", route, securityName)
				}
			}
		}
	}
	undocumented := difference(actual, declared)
	unimplemented := difference(declared, actual)
	if len(undocumented) > 0 || len(unimplemented) > 0 {
		return 0, fmt.Errorf("undocumented: %v; unimplemented: %v", undocumented, unimplemented)
	}
	return len(actual), nil
}

func registeredRoutes(source []byte) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "api.go", source, 0)
	if err != nil {
		return nil, err
	}
	routes := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "mux" || len(call.Args) == 0 {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			err = fmt.Errorf("mux.HandleFunc route must be a string literal")
			return false
		}
		pattern, unquoteErr := strconv.Unquote(literal.Value)
		if unquoteErr != nil {
			err = unquoteErr
			return false
		}
		routes[pattern] = true
		return true
	})
	return routes, err
}

func difference(left, right map[string]bool) []string {
	var result []string
	for route := range left {
		if !right[route] {
			result = append(result, route)
		}
	}
	slices.Sort(result)
	return result
}

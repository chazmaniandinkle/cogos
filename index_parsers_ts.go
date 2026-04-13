package main

// index_parsers_ts.go — Tree-sitter based language parsers for workspace indexing.
//
// Phase 2: Full AST-based symbol extraction for Go, TypeScript, JavaScript,
// Python, Shell, C#. Plus regex-based parsers for Markdown, JSON, YAML.

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	tsTS "github.com/smacker/go-tree-sitter/typescript/typescript"
	tsTSX "github.com/smacker/go-tree-sitter/typescript/tsx"
)

// =============================================================================
// TREE-SITTER PARSER BASE
// =============================================================================

// tsParser is a tree-sitter based parser that uses queries to extract symbols.
type tsParser struct {
	lang       string
	exts       []string
	tsLang     *sitter.Language
	extractors []symbolExtractor
}

// symbolExtractor is a function that walks a tree-sitter node and extracts symbols.
type symbolExtractor func(node *sitter.Node, content []byte, file string) []Symbol

func (p *tsParser) Language() string    { return p.lang }
func (p *tsParser) Extensions() []string { return p.exts }

func (p *tsParser) Parse(path string, content []byte) (*FileRecord, []Symbol, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(p.tsLang)

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, nil, err
	}
	defer tree.Close()

	root := tree.RootNode()

	var symbols []Symbol
	for _, extract := range p.extractors {
		symbols = append(symbols, extract(root, content, path)...)
	}

	// Extract imports
	var imports []string
	for _, imp := range extractImports(root, content, p.lang) {
		imports = append(imports, imp)
	}

	// Extract exports
	var exports []string
	for _, sym := range symbols {
		if sym.Exported {
			exports = append(exports, sym.Name)
		}
	}

	rec := &FileRecord{
		Path:        path,
		Language:    p.lang,
		Size:        int64(len(content)),
		SymbolCount: len(symbols),
		Imports:     imports,
		Exports:     exports,
	}

	return rec, symbols, nil
}

// =============================================================================
// GO PARSER
// =============================================================================

func newGoParser() LanguageParser {
	return &tsParser{
		lang:   "go",
		exts:   []string{".go"},
		tsLang: golang.GetLanguage(),
		extractors: []symbolExtractor{
			extractGoFunctions,
			extractGoTypes,
		},
	}
}

func extractGoFunctions(root *sitter.Node, content []byte, file string) []Symbol {
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		switch node.Type() {
		case "function_declaration":
			name := childByField(node, "name", content)
			params := childByField(node, "parameters", content)
			result := childByField(node, "result", content)
			if name == "" {
				return
			}
			sig := "func " + name + "(" + params + ")"
			if result != "" {
				sig += " " + result
			}
			symbols = append(symbols, Symbol{
				Name:      name,
				Kind:      "function",
				File:      file,
				Line:      int(node.StartPoint().Row) + 1,
				EndLine:   int(node.EndPoint().Row) + 1,
				Language:  "go",
				Signature: sig,
				Exported:  isExported(name),
				DocString: extractGoDoc(node, content),
			})

		case "method_declaration":
			name := childByField(node, "name", content)
			params := childByField(node, "parameters", content)
			receiver := childByField(node, "receiver", content)
			result := childByField(node, "result", content)
			if name == "" {
				return
			}
			// Extract receiver type
			recvType := extractReceiverType(receiver)
			sig := "func (" + receiver + ") " + name + "(" + params + ")"
			if result != "" {
				sig += " " + result
			}
			symbols = append(symbols, Symbol{
				Name:      name,
				Kind:      "method",
				File:      file,
				Line:      int(node.StartPoint().Row) + 1,
				EndLine:   int(node.EndPoint().Row) + 1,
				Language:  "go",
				Scope:     recvType,
				Signature: sig,
				Exported:  isExported(name),
				DocString: extractGoDoc(node, content),
			})
		}
	})
	return symbols
}

func extractGoTypes(root *sitter.Node, content []byte, file string) []Symbol {
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "type_declaration" {
			return
		}
		// type_declaration contains type_spec children
		for i := 0; i < int(node.NamedChildCount()); i++ {
			spec := node.NamedChild(i)
			if spec.Type() != "type_spec" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			typeNode := spec.ChildByFieldName("type")
			if nameNode == nil {
				continue
			}
			name := nodeText(nameNode, content)
			kind := "type"
			if typeNode != nil {
				switch typeNode.Type() {
				case "struct_type":
					kind = "class"
				case "interface_type":
					kind = "interface"
				}
			}
			symbols = append(symbols, Symbol{
				Name:     name,
				Kind:     kind,
				File:     file,
				Line:     int(spec.StartPoint().Row) + 1,
				EndLine:  int(spec.EndPoint().Row) + 1,
				Language: "go",
				Exported: isExported(name),
				DocString: extractGoDoc(node, content),
			})
		}
	})
	return symbols
}

// =============================================================================
// TYPESCRIPT / JAVASCRIPT PARSER
// =============================================================================

func newTypeScriptParser() LanguageParser {
	return &tsParser{
		lang:   "typescript",
		exts:   []string{".ts"},
		tsLang: tsTS.GetLanguage(),
		extractors: []symbolExtractor{
			extractJSFunctions,
			extractJSClasses,
			extractJSArrowFunctions,
		},
	}
}

func newTSXParser() LanguageParser {
	return &tsParser{
		lang:   "typescript",
		exts:   []string{".tsx"},
		tsLang: tsTSX.GetLanguage(),
		extractors: []symbolExtractor{
			extractJSFunctions,
			extractJSClasses,
			extractJSArrowFunctions,
		},
	}
}

func newJavaScriptParser() LanguageParser {
	return &tsParser{
		lang:   "javascript",
		exts:   []string{".js", ".jsx", ".mjs", ".cjs"},
		tsLang: javascript.GetLanguage(),
		extractors: []symbolExtractor{
			extractJSFunctions,
			extractJSClasses,
			extractJSArrowFunctions,
		},
	}
}

func extractJSFunctions(root *sitter.Node, content []byte, file string) []Symbol {
	lang := detectLangFromFile(file)
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "function_declaration" {
			return
		}
		name := childByField(node, "name", content)
		params := childByField(node, "parameters", content)
		if name == "" {
			return
		}
		exported := isJSExported(node)
		symbols = append(symbols, Symbol{
			Name:      name,
			Kind:      "function",
			File:      file,
			Line:      int(node.StartPoint().Row) + 1,
			EndLine:   int(node.EndPoint().Row) + 1,
			Language:  lang,
			Signature: "function " + name + "(" + params + ")",
			Exported:  exported,
		})
	})
	return symbols
}

func extractJSClasses(root *sitter.Node, content []byte, file string) []Symbol {
	lang := detectLangFromFile(file)
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "class_declaration" {
			return
		}
		name := childByField(node, "name", content)
		if name == "" {
			return
		}
		exported := isJSExported(node)
		symbols = append(symbols, Symbol{
			Name:     name,
			Kind:     "class",
			File:     file,
			Line:     int(node.StartPoint().Row) + 1,
			EndLine:  int(node.EndPoint().Row) + 1,
			Language: lang,
			Exported: exported,
		})

		// Extract methods
		body := node.ChildByFieldName("body")
		if body == nil {
			return
		}
		for i := 0; i < int(body.NamedChildCount()); i++ {
			child := body.NamedChild(i)
			if child.Type() == "method_definition" {
				mName := childByField(child, "name", content)
				mParams := childByField(child, "parameters", content)
				if mName == "" {
					continue
				}
				symbols = append(symbols, Symbol{
					Name:      mName,
					Kind:      "method",
					File:      file,
					Line:      int(child.StartPoint().Row) + 1,
					EndLine:   int(child.EndPoint().Row) + 1,
					Language:  lang,
					Scope:     name,
					Signature: mName + "(" + mParams + ")",
					Exported:  exported,
				})
			}
		}
	})
	return symbols
}

func extractJSArrowFunctions(root *sitter.Node, content []byte, file string) []Symbol {
	lang := detectLangFromFile(file)
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		// Match: const/let/var NAME = (...) =>
		if node.Type() != "lexical_declaration" && node.Type() != "variable_declaration" {
			return
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			decl := node.NamedChild(i)
			if decl.Type() != "variable_declarator" {
				continue
			}
			nameNode := decl.ChildByFieldName("name")
			valueNode := decl.ChildByFieldName("value")
			if nameNode == nil || valueNode == nil {
				continue
			}
			if valueNode.Type() != "arrow_function" {
				continue
			}
			name := nodeText(nameNode, content)
			params := childByField(valueNode, "parameters", content)
			exported := isJSExported(node)
			symbols = append(symbols, Symbol{
				Name:      name,
				Kind:      "function",
				File:      file,
				Line:      int(node.StartPoint().Row) + 1,
				EndLine:   int(node.EndPoint().Row) + 1,
				Language:  lang,
				Signature: "const " + name + " = (" + params + ") =>",
				Exported:  exported,
			})
		}
	})
	return symbols
}

// =============================================================================
// PYTHON PARSER
// =============================================================================

func newPythonParser() LanguageParser {
	return &tsParser{
		lang:   "python",
		exts:   []string{".py"},
		tsLang: python.GetLanguage(),
		extractors: []symbolExtractor{
			extractPythonSymbols,
		},
	}
}

func extractPythonSymbols(root *sitter.Node, content []byte, file string) []Symbol {
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		switch node.Type() {
		case "function_definition":
			name := childByField(node, "name", content)
			params := childByField(node, "parameters", content)
			if name == "" {
				return
			}
			// Determine if this is a method (parent is class_definition body)
			kind := "function"
			scope := ""
			if parent := node.Parent(); parent != nil {
				if gp := parent.Parent(); gp != nil && gp.Type() == "class_definition" {
					kind = "method"
					scope = childByField(gp, "name", content)
				}
			}
			symbols = append(symbols, Symbol{
				Name:      name,
				Kind:      kind,
				File:      file,
				Line:      int(node.StartPoint().Row) + 1,
				EndLine:   int(node.EndPoint().Row) + 1,
				Language:  "python",
				Scope:     scope,
				Signature: "def " + name + "(" + params + ")",
				Exported:  !strings.HasPrefix(name, "_"),
				DocString: extractPythonDocstring(node, content),
			})

		case "class_definition":
			name := childByField(node, "name", content)
			if name == "" {
				return
			}
			symbols = append(symbols, Symbol{
				Name:      name,
				Kind:      "class",
				File:      file,
				Line:      int(node.StartPoint().Row) + 1,
				EndLine:   int(node.EndPoint().Row) + 1,
				Language:  "python",
				Exported:  !strings.HasPrefix(name, "_"),
				DocString: extractPythonDocstring(node, content),
			})
		}
	})
	return symbols
}

// =============================================================================
// SHELL PARSER
// =============================================================================

func newShellParser() LanguageParser {
	return &tsParser{
		lang:   "shell",
		exts:   []string{".sh", ".bash", ".zsh"},
		tsLang: bash.GetLanguage(),
		extractors: []symbolExtractor{
			extractShellSymbols,
		},
	}
}

func extractShellSymbols(root *sitter.Node, content []byte, file string) []Symbol {
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "function_definition" {
			return
		}
		name := childByField(node, "name", content)
		if name == "" {
			return
		}
		symbols = append(symbols, Symbol{
			Name:      name,
			Kind:      "function",
			File:      file,
			Line:      int(node.StartPoint().Row) + 1,
			EndLine:   int(node.EndPoint().Row) + 1,
			Language:  "shell",
			Signature: name + "()",
			Exported:  true,
		})
	})
	return symbols
}

// =============================================================================
// C# PARSER
// =============================================================================

func newCSharpParser() LanguageParser {
	return &tsParser{
		lang:   "csharp",
		exts:   []string{".cs"},
		tsLang: csharp.GetLanguage(),
		extractors: []symbolExtractor{
			extractCSharpSymbols,
		},
	}
}

func extractCSharpSymbols(root *sitter.Node, content []byte, file string) []Symbol {
	var symbols []Symbol
	walkNodes(root, func(node *sitter.Node) {
		switch node.Type() {
		case "class_declaration":
			name := childByField(node, "name", content)
			if name == "" {
				return
			}
			symbols = append(symbols, Symbol{
				Name:     name,
				Kind:     "class",
				File:     file,
				Line:     int(node.StartPoint().Row) + 1,
				EndLine:  int(node.EndPoint().Row) + 1,
				Language: "csharp",
				Exported: true,
			})

		case "method_declaration":
			name := childByField(node, "name", content)
			params := childByField(node, "parameters", content)
			if name == "" {
				return
			}
			scope := ""
			if parent := node.Parent(); parent != nil {
				if gp := parent.Parent(); gp != nil && gp.Type() == "class_declaration" {
					scope = childByField(gp, "name", content)
				}
			}
			symbols = append(symbols, Symbol{
				Name:      name,
				Kind:      "method",
				File:      file,
				Line:      int(node.StartPoint().Row) + 1,
				EndLine:   int(node.EndPoint().Row) + 1,
				Language:  "csharp",
				Scope:     scope,
				Signature: name + "(" + params + ")",
				Exported:  true,
			})

		case "property_declaration":
			name := childByField(node, "name", content)
			if name == "" {
				return
			}
			scope := ""
			if parent := node.Parent(); parent != nil {
				if gp := parent.Parent(); gp != nil && gp.Type() == "class_declaration" {
					scope = childByField(gp, "name", content)
				}
			}
			symbols = append(symbols, Symbol{
				Name:     name,
				Kind:     "variable",
				File:     file,
				Line:     int(node.StartPoint().Row) + 1,
				EndLine:  int(node.EndPoint().Row) + 1,
				Language: "csharp",
				Scope:    scope,
				Exported: true,
			})
		}
	})
	return symbols
}

// =============================================================================
// REGEX-BASED PARSERS (Markdown, JSON, YAML)
// =============================================================================

// mdParser extracts headings and links from markdown files.
type mdParser struct{}

func (p *mdParser) Language() string     { return "markdown" }
func (p *mdParser) Extensions() []string { return []string{".md"} }

var reMDHeading = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)`)
var reMDLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

func (p *mdParser) Parse(path string, content []byte) (*FileRecord, []Symbol, error) {
	var symbols []Symbol
	var imports []string
	text := string(content)

	for _, m := range reMDHeading.FindAllStringSubmatchIndex(text, -1) {
		level := m[3] - m[2] // length of # sequence
		heading := text[m[4]:m[5]]
		line := strings.Count(text[:m[0]], "\n") + 1
		symbols = append(symbols, Symbol{
			Name:     strings.TrimSpace(heading),
			Kind:     "heading",
			File:     path,
			Line:     line,
			Language: "markdown",
			Exported: true,
		})
		_ = level
	}

	for _, m := range reMDLink.FindAllStringSubmatch(text, -1) {
		target := m[2]
		if !strings.HasPrefix(target, "http") {
			imports = append(imports, target)
		}
	}

	rec := &FileRecord{
		Path:        path,
		Language:    "markdown",
		Size:        int64(len(content)),
		SymbolCount: len(symbols),
		Imports:     imports,
	}
	return rec, symbols, nil
}

// jsonParser extracts top-level keys from JSON files.
type jsonParser struct{}

func (p *jsonParser) Language() string     { return "json" }
func (p *jsonParser) Extensions() []string { return []string{".json"} }

var reJSONKey = regexp.MustCompile(`(?m)^\s{0,4}"([^"]+)":\s`)

func (p *jsonParser) Parse(path string, content []byte) (*FileRecord, []Symbol, error) {
	var symbols []Symbol

	// Only extract from reasonably sized JSON files
	if len(content) < 100_000 {
		for _, m := range reJSONKey.FindAllStringSubmatchIndex(string(content), 50) {
			key := string(content[m[2]:m[3]])
			line := strings.Count(string(content[:m[0]]), "\n") + 1
			symbols = append(symbols, Symbol{
				Name:     key,
				Kind:     "key",
				File:     path,
				Line:     line,
				Language: "json",
				Exported: true,
			})
		}
	}

	rec := &FileRecord{
		Path:        path,
		Language:    "json",
		Size:        int64(len(content)),
		SymbolCount: len(symbols),
	}
	return rec, symbols, nil
}

// yamlParser extracts top-level keys from YAML files.
type yamlParser struct{}

func (p *yamlParser) Language() string     { return "yaml" }
func (p *yamlParser) Extensions() []string { return []string{".yaml", ".yml"} }

var reYAMLKey = regexp.MustCompile(`(?m)^([a-zA-Z_][a-zA-Z0-9_-]*):\s`)

func (p *yamlParser) Parse(path string, content []byte) (*FileRecord, []Symbol, error) {
	var symbols []Symbol

	for _, m := range reYAMLKey.FindAllStringSubmatchIndex(string(content), 50) {
		key := string(content[m[2]:m[3]])
		line := strings.Count(string(content[:m[0]]), "\n") + 1
		symbols = append(symbols, Symbol{
			Name:     key,
			Kind:     "key",
			File:     path,
			Line:     line,
			Language: "yaml",
			Exported: true,
		})
	}

	rec := &FileRecord{
		Path:        path,
		Language:    "yaml",
		Size:        int64(len(content)),
		SymbolCount: len(symbols),
	}
	return rec, symbols, nil
}

// =============================================================================
// HELPERS
// =============================================================================

// walkNodes performs a depth-first walk of the tree-sitter AST.
func walkNodes(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.NamedChildCount()); i++ {
		walkNodes(node.NamedChild(i), fn)
	}
}

// nodeText returns the text content of a tree-sitter node.
func nodeText(node *sitter.Node, content []byte) string {
	return string(content[node.StartByte():node.EndByte()])
}

// childByField returns the text of a named field, or empty string.
func childByField(node *sitter.Node, field string, content []byte) string {
	child := node.ChildByFieldName(field)
	if child == nil {
		return ""
	}
	return nodeText(child, content)
}

// isExported checks if a Go identifier is exported (starts with uppercase).
func isExported(name string) bool {
	if len(name) == 0 {
		return false
	}
	return name[0] >= 'A' && name[0] <= 'Z'
}

// extractReceiverType extracts the type name from a Go receiver string like "s *Server".
func extractReceiverType(receiver string) string {
	// Remove parens, trim
	r := strings.TrimSpace(receiver)
	r = strings.TrimPrefix(r, "(")
	r = strings.TrimSuffix(r, ")")
	parts := strings.Fields(r)
	if len(parts) >= 2 {
		return strings.TrimPrefix(parts[1], "*")
	}
	if len(parts) == 1 {
		return strings.TrimPrefix(parts[0], "*")
	}
	return ""
}

// extractGoDoc extracts the doc comment preceding a Go declaration.
func extractGoDoc(node *sitter.Node, content []byte) string {
	prev := node.PrevNamedSibling()
	if prev == nil || prev.Type() != "comment" {
		return ""
	}
	text := nodeText(prev, content)
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimSpace(text)
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// extractPythonDocstring extracts the docstring from a Python function or class.
func extractPythonDocstring(node *sitter.Node, content []byte) string {
	body := node.ChildByFieldName("body")
	if body == nil {
		return ""
	}
	if body.NamedChildCount() == 0 {
		return ""
	}
	first := body.NamedChild(0)
	if first.Type() != "expression_statement" {
		return ""
	}
	if first.NamedChildCount() == 0 {
		return ""
	}
	str := first.NamedChild(0)
	if str.Type() != "string" {
		return ""
	}
	text := nodeText(str, content)
	// Trim triple quotes
	text = strings.TrimPrefix(text, `"""`)
	text = strings.TrimPrefix(text, `'''`)
	text = strings.TrimSuffix(text, `"""`)
	text = strings.TrimSuffix(text, `'''`)
	text = strings.TrimSpace(text)
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// isJSExported checks if a JS/TS node is preceded by 'export'.
func isJSExported(node *sitter.Node) bool {
	parent := node.Parent()
	if parent != nil && parent.Type() == "export_statement" {
		return true
	}
	return false
}

// detectLangFromFile determines typescript vs javascript from file extension.
func detectLangFromFile(file string) string {
	ext := filepath.Ext(file)
	switch ext {
	case ".ts", ".tsx":
		return "typescript"
	default:
		return "javascript"
	}
}

// extractImports extracts import paths/modules from the AST.
func extractImports(root *sitter.Node, content []byte, lang string) []string {
	var imports []string
	walkNodes(root, func(node *sitter.Node) {
		switch lang {
		case "go":
			if node.Type() == "import_spec" {
				path := childByField(node, "path", content)
				path = strings.Trim(path, `"`)
				if path != "" {
					imports = append(imports, path)
				}
			}
		case "typescript", "javascript":
			if node.Type() == "import_statement" {
				src := node.ChildByFieldName("source")
				if src != nil {
					path := nodeText(src, content)
					path = strings.Trim(path, `"'`)
					imports = append(imports, path)
				}
			}
		case "python":
			if node.Type() == "import_statement" || node.Type() == "import_from_statement" {
				module := node.ChildByFieldName("module_name")
				if module == nil {
					// Try "name" for plain import
					for i := 0; i < int(node.NamedChildCount()); i++ {
						child := node.NamedChild(i)
						if child.Type() == "dotted_name" {
							imports = append(imports, nodeText(child, content))
						}
					}
				} else {
					imports = append(imports, nodeText(module, content))
				}
			}
		case "shell":
			if node.Type() == "command" {
				nameNode := node.ChildByFieldName("name")
				if nameNode != nil && (nodeText(nameNode, content) == "source" || nodeText(nameNode, content) == ".") {
					for i := 0; i < int(node.NamedChildCount()); i++ {
						arg := node.NamedChild(i)
						if arg.Type() == "word" || arg.Type() == "string" {
							text := nodeText(arg, content)
							text = strings.Trim(text, `"'`)
							if text != "source" && text != "." {
								imports = append(imports, text)
							}
						}
					}
				}
			}
		}
	})
	return imports
}

// =============================================================================
// CALL GRAPH EXTRACTION
// =============================================================================

// ExtractCalls implements CallGraphParser for tree-sitter parsers.
// Returns a map of caller → []callee within the file.
func (p *tsParser) ExtractCalls(path string, content []byte) map[string][]string {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(p.tsLang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil
	}
	defer tree.Close()
	return extractCallEdges(tree.RootNode(), content, path, p.lang)
}

// extractCallEdges walks the AST and builds a caller → []callee map.
func extractCallEdges(root *sitter.Node, content []byte, file string, lang string) map[string][]string {
	calls := make(map[string][]string)

	walkNodes(root, func(node *sitter.Node) {
		// Match call expression nodes by language
		if !isCallNode(node, lang) {
			return
		}

		callee := resolveCalleeName(node, content, lang)
		if callee == "" || callee == "_" {
			return
		}

		caller := findEnclosingFunction(node, content)
		if caller == "" {
			caller = "<module>"
		}

		calls[caller] = appendUnique(calls[caller], callee)
	})

	return calls
}

// isCallNode returns true if the node represents a function call for the given language.
func isCallNode(node *sitter.Node, lang string) bool {
	switch lang {
	case "go", "typescript", "javascript":
		return node.Type() == "call_expression"
	case "python":
		return node.Type() == "call"
	case "csharp":
		return node.Type() == "invocation_expression"
	case "shell":
		return node.Type() == "command"
	}
	return false
}

// resolveCalleeName extracts the function/method name being called.
func resolveCalleeName(node *sitter.Node, content []byte, lang string) string {
	if lang == "shell" {
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			return nodeText(nameNode, content)
		}
		return ""
	}

	funcNode := node.ChildByFieldName("function")
	if funcNode == nil {
		return ""
	}

	switch funcNode.Type() {
	case "identifier":
		return nodeText(funcNode, content)
	case "selector_expression": // Go: pkg.Func or receiver.Method
		field := funcNode.ChildByFieldName("field")
		if field != nil {
			return nodeText(field, content)
		}
	case "member_expression": // JS/TS: obj.method
		prop := funcNode.ChildByFieldName("property")
		if prop != nil {
			return nodeText(prop, content)
		}
	case "attribute": // Python: obj.method
		attr := funcNode.ChildByFieldName("attribute")
		if attr != nil {
			return nodeText(attr, content)
		}
	}

	// Fallback: use full text (truncated)
	text := nodeText(funcNode, content)
	if len(text) > 60 {
		return ""
	}
	return text
}

// findEnclosingFunction walks up the parent chain to find the nearest function declaration.
func findEnclosingFunction(node *sitter.Node, content []byte) string {
	for p := node.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "function_declaration":
			return childByField(p, "name", content)
		case "method_declaration":
			name := childByField(p, "name", content)
			recv := extractReceiverType(childByField(p, "receiver", content))
			if recv != "" {
				return recv + "." + name
			}
			return name
		case "function_definition": // Python
			name := childByField(p, "name", content)
			// Check if it's a method (inside class body)
			if pp := p.Parent(); pp != nil {
				if gp := pp.Parent(); gp != nil && gp.Type() == "class_definition" {
					className := childByField(gp, "name", content)
					if className != "" {
						return className + "." + name
					}
				}
			}
			return name
		case "method_definition": // JS/TS class methods
			name := childByField(p, "name", content)
			// Find parent class
			if pp := p.Parent(); pp != nil {
				if gp := pp.Parent(); gp != nil && gp.Type() == "class_declaration" {
					className := childByField(gp, "name", content)
					if className != "" {
						return className + "." + name
					}
				}
			}
			return name
		case "arrow_function":
			// Check if assigned to a variable
			if pp := p.Parent(); pp != nil && pp.Type() == "variable_declarator" {
				nameNode := pp.ChildByFieldName("name")
				if nameNode != nil {
					return nodeText(nameNode, content)
				}
			}
		}
	}
	return ""
}

// appendUnique appends s to slice only if not already present.
func appendUnique(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}

// =============================================================================
// REGISTRY BUILDER
// =============================================================================

// buildDefaultRegistry creates the registry with all tree-sitter + regex parsers.
func buildDefaultRegistry() *LanguageRegistry {
	reg := NewLanguageRegistry()
	reg.Register(newGoParser())
	reg.Register(newTypeScriptParser())
	reg.Register(newTSXParser())
	reg.Register(newJavaScriptParser())
	reg.Register(newPythonParser())
	reg.Register(newShellParser())
	reg.Register(newCSharpParser())
	reg.Register(&mdParser{})
	reg.Register(&jsonParser{})
	reg.Register(&yamlParser{})
	return reg
}

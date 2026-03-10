package main

// index_parsers.go — Language parser interface and registry for workspace indexing.
//
// Phase 0: Defines interfaces. Phase 2 adds tree-sitter implementations.

// LanguageParser extracts symbols and metadata from a source file.
type LanguageParser interface {
	// Parse extracts symbols and file metadata from source content.
	// path is relative to workspace root, content is the raw file bytes.
	Parse(path string, content []byte) (*FileRecord, []Symbol, error)

	// Language returns the language name this parser handles.
	Language() string

	// Extensions returns file extensions this parser handles (e.g. ".go", ".py").
	Extensions() []string
}

// LanguageRegistry maps file extensions to their parsers.
type LanguageRegistry struct {
	parsers    map[string]LanguageParser // extension → parser
	byLanguage map[string]LanguageParser // language name → parser
}

// NewLanguageRegistry creates an empty registry.
func NewLanguageRegistry() *LanguageRegistry {
	return &LanguageRegistry{
		parsers:    make(map[string]LanguageParser),
		byLanguage: make(map[string]LanguageParser),
	}
}

// Register adds a parser for its declared extensions.
func (r *LanguageRegistry) Register(p LanguageParser) {
	for _, ext := range p.Extensions() {
		r.parsers[ext] = p
	}
	r.byLanguage[p.Language()] = p
}

// ForExtension returns the parser for a given file extension, or nil.
func (r *LanguageRegistry) ForExtension(ext string) LanguageParser {
	return r.parsers[ext]
}

// ForLanguage returns the parser for a given language name, or nil.
func (r *LanguageRegistry) ForLanguage(lang string) LanguageParser {
	return r.byLanguage[lang]
}

// Languages returns all registered language names.
func (r *LanguageRegistry) Languages() []string {
	langs := make([]string, 0, len(r.byLanguage))
	for lang := range r.byLanguage {
		langs = append(langs, lang)
	}
	return langs
}

// CallGraphParser is an optional interface for parsers that can extract call edges.
// Not all parsers support this (e.g. markdown, JSON, YAML parsers don't).
type CallGraphParser interface {
	// ExtractCalls parses the file and returns a call graph: caller → []callee.
	ExtractCalls(path string, content []byte) map[string][]string
}

// defaultRegistry builds the registry with all available parsers.
func defaultRegistry() *LanguageRegistry {
	return buildDefaultRegistry()
}

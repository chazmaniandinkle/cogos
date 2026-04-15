// Package uri implements resolution of cog:// URIs to concrete resources.
//
// The cog:// scheme provides a location-independent addressing system for
// memory sectors, ontology entries, agent definitions, and other workspace
// resources. This package parses URIs, resolves them against the workspace
// layout, and returns readers for the underlying content.
package uri

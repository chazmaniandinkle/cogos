// cmd_registry.go - Unified registry CLI for operational state
//
// Provides a state-management view over constellation-indexed registry documents.
// While `cog constellation search` does FTS across all knowledge, `cog registry`
// focuses on operational resources: fleets, agents, services, research, coordination.

package main

import (
	"fmt"
	"os"
	"strings"
)

// cmdRegistry handles registry subcommands.
func cmdRegistry(args []string) error {
	if len(args) == 0 {
		fmt.Println("Usage: cog registry {types|list|search|show|index}")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  types            List all document types with counts")
		fmt.Println("  list [type]      List documents of a given type")
		fmt.Println("  search <query>   Search registry entries")
		fmt.Println("  show <id>        Show document details + edges")
		fmt.Println("  index            Index all registries into constellation")
		return nil
	}

	workspaceRoot, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("failed to resolve workspace: %w", err)
	}

	switch args[0] {
	case "types":
		return registryTypes()
	case "list":
		typeName := ""
		if len(args) > 1 {
			typeName = args[1]
		}
		return registryList(typeName)
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("search requires a query argument")
		}
		return registrySearch(args[1])
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("show requires a document ID")
		}
		return registryShow(args[1])
	case "index":
		return registryIndex(workspaceRoot)
	default:
		return fmt.Errorf("unknown registry subcommand: %s", args[0])
	}
}

// registryTypes shows all document types with counts.
func registryTypes() error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	rows, err := c.DB().Query(`
		SELECT type, COUNT(*) as cnt
		FROM documents
		GROUP BY type
		ORDER BY cnt DESC
	`)
	if err != nil {
		return fmt.Errorf("query types: %w", err)
	}
	defer rows.Close()

	fmt.Printf("%-25s %6s\n", "TYPE", "COUNT")
	fmt.Println("------------------------- ------")

	for rows.Next() {
		var docType string
		var count int
		if err := rows.Scan(&docType, &count); err != nil {
			continue
		}
		fmt.Printf("%-25s %6d\n", docType, count)
	}

	return nil
}

// registryList shows all documents of a given type.
func registryList(typeName string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	var rows interface{ Next() bool }
	var scanner interface {
		Scan(...interface{}) error
	}

	query := `SELECT id, title, updated FROM documents`
	args := []interface{}{}

	if typeName != "" {
		query += ` WHERE type = ?`
		args = append(args, typeName)
	}
	query += ` ORDER BY updated DESC LIMIT 50`

	sqlRows, err := c.DB().Query(query, args...)
	if err != nil {
		return fmt.Errorf("query list: %w", err)
	}
	defer sqlRows.Close()
	rows = sqlRows

	fmt.Printf("%-35s %-50s %s\n", "ID", "TITLE", "UPDATED")
	fmt.Println(strings.Repeat("-", 35) + " " + strings.Repeat("-", 50) + " " + strings.Repeat("-", 20))

	_ = rows
	_ = scanner
	for sqlRows.Next() {
		var id, title, updated string
		if err := sqlRows.Scan(&id, &title, &updated); err != nil {
			continue
		}
		if len(id) > 35 {
			id = id[:32] + "..."
		}
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		if len(updated) > 20 {
			updated = updated[:20]
		}
		fmt.Printf("%-35s %-50s %s\n", id, title, updated)
	}

	return nil
}

// registrySearch searches registry entries using FTS.
func registrySearch(query string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	// Use constellation's search but filter to operational sector
	results, err := c.Search(query, 20)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return nil
	}

	fmt.Printf("Found %d results:\n\n", len(results))
	for i, node := range results {
		fmt.Printf("%d. %s\n", i+1, node.Title)
		fmt.Printf("   Type: %-20s Sector: %s\n", node.Type, node.Sector)
		fmt.Printf("   ID: %s\n", node.ID)
		fmt.Printf("   Rank: %.2f\n\n", node.Rank)
	}

	return nil
}

// registryShow shows a document's details and its edges.
func registryShow(docID string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	// Get document
	row := c.DB().QueryRow(`
		SELECT id, path, type, title, sector, created, updated, content
		FROM documents WHERE id = ?
	`, docID)

	var id, path, docType, title, sector, created, updated, content string
	if err := row.Scan(&id, &path, &docType, &title, &sector, &created, &updated, &content); err != nil {
		return fmt.Errorf("document not found: %s", docID)
	}

	fmt.Printf("ID:      %s\n", id)
	fmt.Printf("Type:    %s\n", docType)
	fmt.Printf("Title:   %s\n", title)
	fmt.Printf("Path:    %s\n", path)
	fmt.Printf("Sector:  %s\n", sector)
	fmt.Printf("Created: %s\n", created)
	fmt.Printf("Updated: %s\n", updated)

	// Show content (truncated)
	if len(content) > 500 {
		content = content[:500] + "\n..."
	}
	fmt.Printf("\n--- Content ---\n%s\n", content)

	// Show outgoing edges
	edgeRows, err := c.DB().Query(`
		SELECT target_uri, relation FROM doc_references WHERE source_id = ?
	`, docID)
	if err == nil {
		defer edgeRows.Close()
		first := true
		for edgeRows.Next() {
			if first {
				fmt.Printf("\n--- Edges (outgoing) ---\n")
				first = false
			}
			var targetURI, relation string
			edgeRows.Scan(&targetURI, &relation)
			fmt.Printf("  -[%s]-> %s\n", relation, targetURI)
		}
	}

	// Show incoming edges (backlinks)
	blRows, err := c.DB().Query(`
		SELECT source_id, relation FROM doc_references WHERE target_uri = ? OR target_id = ?
	`, docID, docID)
	if err == nil {
		defer blRows.Close()
		first := true
		for blRows.Next() {
			if first {
				fmt.Printf("\n--- Edges (incoming) ---\n")
				first = false
			}
			var sourceID, relation string
			blRows.Scan(&sourceID, &relation)
			fmt.Printf("  <-[%s]- %s\n", relation, sourceID)
		}
	}

	return nil
}

// registryIndex indexes all registries into constellation.
func registryIndex(workspaceRoot string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	fmt.Println("Indexing all registries...")
	total, err := indexAllRegistries(c, workspaceRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
	fmt.Printf("Total: %d registry entries indexed\n", total)

	return nil
}

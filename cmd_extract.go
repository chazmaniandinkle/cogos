// cmd_extract.go — CLI command for cog extract.
// ADR-040 Phase 1: build-time code extraction from cogdocs.

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdExtract implements the "cog extract" command.
func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	allFlag := fs.Bool("all", false, "Extract from all .cog.md files under .cog/hooks/")
	dryRun := fs.Bool("dry-run", false, "Show what would be extracted without writing")
	outDir := fs.String("outdir", ".", "Output directory for extracted files")

	if err := fs.Parse(args); err != nil {
		return err
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	var files []string

	if *allFlag {
		// Walk .cog/hooks/ for .cog.md files
		hooksDir := filepath.Join(root, ".cog", "hooks")
		err := filepath.Walk(hooksDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip errors
			}
			if !info.IsDir() && strings.HasSuffix(path, ".cog.md") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk hooks directory: %w", err)
		}
		if len(files) == 0 {
			fmt.Println("No .cog.md files found in .cog/hooks/")
			return nil
		}
	} else {
		// Expect file arguments
		remaining := fs.Args()
		if len(remaining) == 0 {
			fmt.Fprintf(os.Stderr, "Usage: cog extract [--all] [--dry-run] [--outdir=DIR] <file.cog.md> ...\n")
			return fmt.Errorf("no input files specified")
		}
		for _, f := range remaining {
			// Resolve relative to cwd, not workspace root
			if !filepath.IsAbs(f) {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("getwd: %w", err)
				}
				f = filepath.Join(cwd, f)
			}
			files = append(files, f)
		}
	}

	// Resolve outDir
	resolvedOutDir := *outDir
	if !filepath.IsAbs(resolvedOutDir) {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		resolvedOutDir = filepath.Join(cwd, resolvedOutDir)
	}

	totalBlocks := 0
	var allWritten []string

	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}

		// Use relative source name for the generation header
		relSource, err := filepath.Rel(root, file)
		if err != nil {
			relSource = file
		}

		blocks, err := ParseExtractBlocks(content, relSource)
		if err != nil {
			return fmt.Errorf("parse %s: %w", file, err)
		}

		if len(blocks) == 0 {
			continue
		}

		totalBlocks += len(blocks)

		written, err := WriteExtractBlocks(blocks, resolvedOutDir, *dryRun)
		if err != nil {
			return fmt.Errorf("write blocks from %s: %w", file, err)
		}
		allWritten = append(allWritten, written...)
	}

	if totalBlocks == 0 {
		fmt.Println("No extract blocks found.")
		return nil
	}

	action := "Extracted"
	if *dryRun {
		action = "Would extract"
	}

	fmt.Printf("%s %d block(s):\n", action, totalBlocks)
	for _, p := range allWritten {
		rel, err := filepath.Rel(resolvedOutDir, p)
		if err != nil {
			rel = p
		}
		fmt.Printf("  %s\n", rel)
	}

	return nil
}

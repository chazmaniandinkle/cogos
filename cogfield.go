// cogfield.go - CogField graph endpoint
//
// Serves the full workspace knowledge graph from constellation.db
// for the CogField visualization frontend.
//
// GET /api/cogfield/graph - Returns all nodes + edges + stats
//
// Type normalization: 64 document types → 11 CogField entity types
// Sector inference: path-based when DB sector is NULL (98% of docs)

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// CogFieldNode matches the frontend CogFieldNode interface
type CogFieldNode struct {
	ID           string                 `json:"id"`
	Label        string                 `json:"label"`
	EntityType   string                 `json:"entity_type"`
	Sector       string                 `json:"sector"`
	Tags         []string               `json:"tags"`
	Created      string                 `json:"created"`
	Modified     string                 `json:"modified"`
	BackrefCount int                    `json:"backref_count"`
	Strength     float64                `json:"strength"`
	Meta         map[string]interface{} `json:"meta,omitempty"`
}

// CogFieldEdge matches the frontend CogFieldEdge interface
type CogFieldEdge struct {
	Source   string  `json:"source"`
	Target   string  `json:"target"`
	Relation string  `json:"relation"`
	Weight   float64 `json:"weight,omitempty"`
	Thread   string  `json:"thread"`
}

// CogFieldStats matches the frontend CogFieldStats interface
type CogFieldStats struct {
	TotalNodes      int            `json:"total_nodes"`
	TotalEdges      int            `json:"total_edges"`
	NodesByType     map[string]int `json:"nodes_by_type"`
	NodesBySector   map[string]int `json:"nodes_by_sector"`
	EdgesByRelation map[string]int `json:"edges_by_relation"`
	EdgesByThread   map[string]int `json:"edges_by_thread"`
	MostConnected   []string       `json:"most_connected"`
}

// CogFieldGraph matches the frontend CogFieldGraph interface
type CogFieldGraph struct {
	Nodes []CogFieldNode `json:"nodes"`
	Edges []CogFieldEdge `json:"edges"`
	Stats CogFieldStats  `json:"stats"`
}

// normalizeEntityType maps constellation.db document types to CogField entity types
func normalizeEntityType(docType string) string {
	switch docType {
	case "session":
		return "session"
	case "adr":
		return "adr"
	case "skill":
		return "skill"
	case "hook":
		return "hook"
	case "ontology", "term", "claim", "pattern", "theorem", "principle":
		return "ontology"
	case "identity":
		return "agent"
	default:
		return "document"
	}
}

// inferSector determines the sector from the document path when DB sector is NULL
func inferSector(path, dbSector string) string {
	// Use DB sector if it's meaningful
	if dbSector != "" {
		// Normalize the DB sector values
		switch dbSector {
		case "semantic", "episodic", "procedural", "reflective",
			"identities", "reference", "temporal", "emotional", "waypoints":
			return dbSector
		case "architecture", "semantic/architecture":
			return "architecture"
		}
	}

	// Infer from path
	p := strings.ToLower(path)

	// Memory sectors
	if strings.Contains(p, "/semantic/") || strings.HasPrefix(p, "semantic/") {
		// Sub-sector detection
		if strings.Contains(p, "/architecture/") {
			return "architecture"
		}
		return "semantic"
	}
	if strings.Contains(p, "/episodic/") || strings.HasPrefix(p, "episodic/") {
		return "episodic"
	}
	if strings.Contains(p, "/procedural/") || strings.HasPrefix(p, "procedural/") {
		return "procedural"
	}
	if strings.Contains(p, "/reflective/") || strings.HasPrefix(p, "reflective/") {
		return "reflective"
	}
	if strings.Contains(p, "/identities/") || strings.HasPrefix(p, "identities/") {
		return "identities"
	}
	if strings.Contains(p, "/reference/") || strings.HasPrefix(p, "reference/") {
		return "reference"
	}

	// Non-memory paths
	if strings.Contains(p, "/adr/") || strings.HasPrefix(p, "adr/") {
		return "architecture"
	}
	if strings.Contains(p, "/ontology/") || strings.HasPrefix(p, "ontology/") {
		return "ontology"
	}

	// Default
	return "semantic"
}

// strengthFromMetrics calculates a 0-10 strength value from substance metrics
func strengthFromMetrics(substanceRatio float64, refCount, wordCount int) float64 {
	// Base from substance ratio (0-4)
	strength := substanceRatio * 4.0

	// Bonus from reference density (0-3)
	if refCount > 10 {
		strength += 3.0
	} else if refCount > 5 {
		strength += 2.0
	} else if refCount > 0 {
		strength += 1.0
	}

	// Bonus from content length (0-3)
	if wordCount > 1000 {
		strength += 3.0
	} else if wordCount > 300 {
		strength += 2.0
	} else if wordCount > 50 {
		strength += 1.0
	}

	if strength > 10 {
		strength = 10
	}
	return strength
}

// handleCogFieldGraph handles GET /api/cogfield/graph
func (s *serveServer) handleCogFieldGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	// Resolve workspace root
	root, _, err := ResolveWorkspace()
	if err != nil {
		http.Error(w, "Failed to resolve workspace", http.StatusInternalServerError)
		return
	}

	// Open constellation DB
	c, err := constellation.Open(root)
	if err != nil {
		log.Printf("cogfield: failed to open constellation: %v", err)
		http.Error(w, "Failed to open constellation database", http.StatusInternalServerError)
		return
	}
	defer c.Close()

	graph, err := buildCogFieldGraph(c)
	if err != nil {
		log.Printf("cogfield: failed to build graph: %v", err)
		http.Error(w, "Failed to build graph", http.StatusInternalServerError)
		return
	}

	log.Printf("cogfield: built graph with %d nodes, %d edges in %v",
		graph.Stats.TotalNodes, graph.Stats.TotalEdges, time.Since(start))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=30") // Cache for 30s
	json.NewEncoder(w).Encode(graph)
}

// buildCogFieldGraph queries constellation.db and assembles the full graph
func buildCogFieldGraph(c *constellation.Constellation) (*CogFieldGraph, error) {
	db := c.DB()

	// --- Query all documents ---
	rows, err := db.Query(`
		SELECT
			d.id,
			d.path,
			COALESCE(d.title, ''),
			COALESCE(d.type, ''),
			COALESCE(d.sector, ''),
			COALESCE(d.created, ''),
			COALESCE(d.updated, ''),
			COALESCE(d.word_count, 0),
			COALESCE(d.substance_ratio, 0.0),
			COALESCE(d.ref_count, 0),
			(SELECT COUNT(*) FROM backlinks b WHERE b.target_id = d.id)
		FROM documents d
		WHERE COALESCE(d.status, '') != 'deprecated'
		ORDER BY d.updated DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()

	nodeMap := make(map[string]*CogFieldNode)
	pathMap := make(map[string]string)  // docID → path (for sibling computation)
	dateMap := make(map[string]string)  // docID → date YYYY-MM-DD (for temporal)
	var nodes []CogFieldNode

	for rows.Next() {
		var (
			id, path, title, docType, sector string
			created, updated                 string
			wordCount, refCount, backrefCount int
			substanceRatio                    float64
		)

		if err := rows.Scan(&id, &path, &title, &docType, &sector,
			&created, &updated, &wordCount, &substanceRatio, &refCount, &backrefCount); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}

		// Use title, fallback to filename
		label := title
		if label == "" {
			parts := strings.Split(path, "/")
			label = parts[len(parts)-1]
			label = strings.TrimSuffix(label, ".cog.md")
			label = strings.TrimSuffix(label, ".md")
			label = strings.ReplaceAll(label, "-", " ")
		}

		node := CogFieldNode{
			ID:           id,
			Label:        label,
			EntityType:   normalizeEntityType(docType),
			Sector:       inferSector(path, sector),
			Tags:         []string{},
			Created:      created,
			Modified:     updated,
			BackrefCount: backrefCount,
			Strength:     strengthFromMetrics(substanceRatio, refCount, wordCount),
		}

		// Preserve original type in meta if it was normalized
		if docType != "" && docType != node.EntityType {
			node.Meta = map[string]interface{}{
				"doc_type": docType,
			}
		}

		nodes = append(nodes, node)
		nodeMap[id] = &nodes[len(nodes)-1]
		pathMap[id] = path
		if len(created) >= 10 {
			dateMap[id] = created[:10]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate documents: %w", err)
	}

	// --- Query tags ---
	tagRows, err := db.Query(`SELECT document_id, tag FROM tags`)
	if err != nil {
		return nil, fmt.Errorf("query tags: %w", err)
	}
	defer tagRows.Close()

	for tagRows.Next() {
		var docID, tag string
		if err := tagRows.Scan(&docID, &tag); err != nil {
			continue
		}
		if node, ok := nodeMap[docID]; ok {
			node.Tags = append(node.Tags, tag)
		}
	}

	// --- Thread 1: explicit edges from doc_references ---
	edgeRows, err := db.Query(`
		SELECT source_id, target_id, COALESCE(relation, 'refs')
		FROM doc_references
		WHERE target_id IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer edgeRows.Close()

	var edges []CogFieldEdge
	edgesByRelation := make(map[string]int)

	for edgeRows.Next() {
		var source, target, relation string
		if err := edgeRows.Scan(&source, &target, &relation); err != nil {
			continue
		}

		// Only include edges where both endpoints exist
		if _, srcOK := nodeMap[source]; !srcOK {
			continue
		}
		if _, tgtOK := nodeMap[target]; !tgtOK {
			continue
		}

		// Normalize hyphenated relation types
		relation = strings.ReplaceAll(relation, "-", "_")

		edges = append(edges, CogFieldEdge{
			Source:   source,
			Target:   target,
			Relation: relation,
			Weight:   1.0,
			Thread:   "explicit",
		})
		edgesByRelation[relation]++
	}

	// --- Thread 2: shared_tags (docs sharing 2+ non-date tags) ---
	tagEdgeRows, err := db.Query(`
		SELECT t1.document_id, t2.document_id, COUNT(*) as n
		FROM tags t1
		JOIN tags t2 ON t1.tag = t2.tag AND t1.document_id < t2.document_id
		WHERE t1.tag NOT GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
		GROUP BY t1.document_id, t2.document_id
		HAVING n >= 2
		ORDER BY n DESC
		LIMIT 5000
	`)
	if err != nil {
		log.Printf("cogfield: shared_tags query failed: %v", err)
	} else {
		defer tagEdgeRows.Close()
		for tagEdgeRows.Next() {
			var doc1, doc2 string
			var shared int
			if err := tagEdgeRows.Scan(&doc1, &doc2, &shared); err != nil {
				continue
			}
			if _, ok1 := nodeMap[doc1]; !ok1 {
				continue
			}
			if _, ok2 := nodeMap[doc2]; !ok2 {
				continue
			}
			weight := math.Min(float64(shared)*0.15, 1.0)
			edges = append(edges, CogFieldEdge{
				Source:   doc1,
				Target:   doc2,
				Relation: "shared_tags",
				Weight:   weight,
				Thread:   "shared_tags",
			})
		}
	}

	// --- Thread 3: siblings (same parent directory, groups of 2-19) ---
	dirGroups := make(map[string][]string) // parent dir → doc IDs
	for id, p := range pathMap {
		if _, ok := nodeMap[id]; !ok {
			continue
		}
		dir := filepath.Dir(p)
		dirGroups[dir] = append(dirGroups[dir], id)
	}
	for _, group := range dirGroups {
		if len(group) < 2 || len(group) >= 20 {
			continue
		}
		sort.Strings(group) // deterministic ordering
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				edges = append(edges, CogFieldEdge{
					Source:   group[i],
					Target:   group[j],
					Relation: "sibling",
					Weight:   0.4,
					Thread:   "siblings",
				})
			}
		}
	}

	// --- Thread 4: temporal (created same day, groups of 2-30) ---
	dateGroups := make(map[string][]string) // date → doc IDs
	for id, date := range dateMap {
		if _, ok := nodeMap[id]; !ok {
			continue
		}
		if date != "" {
			dateGroups[date] = append(dateGroups[date], id)
		}
	}
	for _, group := range dateGroups {
		if len(group) < 2 || len(group) > 30 {
			continue
		}
		sort.Strings(group)
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				edges = append(edges, CogFieldEdge{
					Source:   group[i],
					Target:   group[j],
					Relation: "temporal",
					Weight:   0.15,
					Thread:   "temporal",
				})
			}
		}
	}

	// --- Build stats ---
	nodesByType := make(map[string]int)
	nodesBySector := make(map[string]int)
	edgesByThread := make(map[string]int)
	type scored struct {
		id    string
		score int
	}
	var topNodes []scored

	for i := range nodes {
		nodesByType[nodes[i].EntityType]++
		nodesBySector[nodes[i].Sector]++
		topNodes = append(topNodes, scored{nodes[i].ID, nodes[i].BackrefCount})
	}

	for i := range edges {
		edgesByThread[edges[i].Thread]++
	}

	// Find top 10 most connected
	sort.Slice(topNodes, func(i, j int) bool {
		return topNodes[i].score > topNodes[j].score
	})
	mostConnected := make([]string, 0, 10)
	for i := 0; i < len(topNodes) && i < 10; i++ {
		if topNodes[i].score > 0 {
			mostConnected = append(mostConnected, topNodes[i].id)
		}
	}

	stats := CogFieldStats{
		TotalNodes:      len(nodes),
		TotalEdges:      len(edges),
		NodesByType:     nodesByType,
		NodesBySector:   nodesBySector,
		EdgesByRelation: edgesByRelation,
		EdgesByThread:   edgesByThread,
		MostConnected:   mostConnected,
	}

	return &CogFieldGraph{
		Nodes: nodes,
		Edges: edges,
		Stats: stats,
	}, nil
}

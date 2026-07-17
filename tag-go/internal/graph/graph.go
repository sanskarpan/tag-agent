// Package graph is the local (no-LLM) entity knowledge graph over stored
// memories. Port of src/tag/entity_graph.py — capitalized-phrase + tech-keyword
// extraction, co-occurrence relations, and union-find community detection.
package graph

import (
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tag-agent/tag/internal/store"
)

var techKeywords = map[string]bool{
	"python": true, "javascript": true, "typescript": true, "rust": true, "go": true,
	"java": true, "ruby": true, "c++": true, "redis": true, "postgres": true,
	"postgresql": true, "mysql": true, "sqlite": true, "mongodb": true, "kafka": true,
	"docker": true, "kubernetes": true, "k8s": true, "terraform": true, "ansible": true,
	"nginx": true, "fastapi": true, "django": true, "flask": true, "react": true,
	"vue": true, "angular": true, "node": true, "nodejs": true, "graphql": true,
	"grpc": true, "rest": true, "openai": true, "anthropic": true, "claude": true,
	"gpt": true, "llm": true, "ai": true, "ml": true, "github": true, "gitlab": true,
	"bitbucket": true, "jenkins": true, "circleci": true, "github actions": true,
	"aws": true, "gcp": true, "azure": true, "lambda": true, "s3": true, "ec2": true,
	"ecs": true, "gke": true, "aks": true, "linear": true, "jira": true, "slack": true,
	"notion": true, "figma": true,
}

var (
	capPhraseRe = regexp.MustCompile(`\b[A-Z][a-zA-Z]+(?:\s+[A-Z][a-zA-Z]+)*\b`)
	personRe    = regexp.MustCompile(`^[A-Z][a-z]+ [A-Z][a-z]+$`)
)

type rawEntity struct {
	Name       string
	Type       string
	Confidence float64
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ExtractEntities pulls candidate entities from text (tech keywords + capitalized
// phrases), deduped case-insensitively, preserving discovery order.
func ExtractEntities(content string) []rawEntity {
	var found []rawEntity
	lower := strings.ToLower(content)
	// deterministic keyword order for stable output
	kws := make([]string, 0, len(techKeywords))
	for kw := range techKeywords {
		kws = append(kws, kw)
	}
	sort.Strings(kws)
	for _, kw := range kws {
		if strings.Contains(lower, kw) {
			name := kw
			if kw[0] >= 'a' && kw[0] <= 'z' {
				name = strings.Title(kw) //nolint:staticcheck // Title is fine for ASCII keywords
			}
			found = append(found, rawEntity{Name: name, Type: "technology", Confidence: 0.8})
		}
	}
	for _, phrase := range capPhraseRe.FindAllString(content, -1) {
		if len(phrase) < 3 || techKeywords[strings.ToLower(phrase)] {
			continue
		}
		etype := "other"
		switch {
		case strings.Contains(phrase, "Inc"), strings.Contains(phrase, "Corp"),
			strings.Contains(phrase, "Ltd"), strings.Contains(phrase, "LLC"),
			strings.Contains(phrase, "GmbH"):
			etype = "organization"
		case personRe.MatchString(phrase):
			etype = "person"
		}
		found = append(found, rawEntity{Name: phrase, Type: etype, Confidence: 0.6})
	}
	seen := map[string]bool{}
	var deduped []rawEntity
	for _, e := range found {
		k := strings.ToLower(e.Name)
		if !seen[k] {
			seen[k] = true
			deduped = append(deduped, e)
		}
	}
	return deduped
}

// addEntity upserts an entity by (profile, name COLLATE NOCASE), bumping
// mention_count and keeping the max confidence. Returns the entity id.
func addEntity(db *store.DB, name, etype, profile string, confidence float64) (string, error) {
	var id string
	var count int
	var conf float64
	err := db.QueryRow(`SELECT id, mention_count, confidence FROM entities WHERE profile=? AND name=? COLLATE NOCASE`,
		profile, name).Scan(&id, &count, &conf)
	if err == nil {
		newConf := conf
		if confidence > newConf {
			newConf = confidence
		}
		_, err = db.Exec(`UPDATE entities SET mention_count=?, confidence=? WHERE id=?`, count+1, newConf, id)
		return id, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = uuid.NewString()[:12]
	_, err = db.Exec(`INSERT INTO entities(id,name,entity_type,description,confidence,profile,created_at,mention_count)
		VALUES(?,?,?,'',?,?,?,1)`, id, name, etype, confidence, profile, now())
	return id, err
}

// addRelation idempotently inserts a (source,target,type) relation.
func addRelation(db *store.DB, srcID, tgtID, relType string, confidence float64, memoryID string) error {
	var existing string
	err := db.QueryRow(`SELECT id FROM relations WHERE source_entity_id=? AND target_entity_id=? AND relation_type=?`,
		srcID, tgtID, relType).Scan(&existing)
	if err == nil {
		return nil // already present
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = db.Exec(`INSERT INTO relations(id,source_entity_id,target_entity_id,relation_type,confidence,source_memory_id,created_at)
		VALUES(?,?,?,?,?,?,?)`, uuid.NewString()[:12], srcID, tgtID, relType, confidence, memoryID, now())
	return err
}

// Reset clears all graph state for a profile (idempotent rebuild — C021).
func Reset(db *store.DB, profile string) error {
	rows, err := db.Query(`SELECT id FROM entities WHERE profile=?`, profile)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if _, err := db.Exec(`DELETE FROM relations WHERE source_entity_id=? OR target_entity_id=?`, id, id); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`DELETE FROM entities WHERE profile=?`, profile); err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM entity_communities WHERE profile=?`, profile)
	return err
}

// ExtractAndStore extracts entities + co-occurrence relations from one memory.
// Returns (entitiesAdded, relationsAdded).
func ExtractAndStore(db *store.DB, memoryID, content, profile string) (int, int, error) {
	raw := ExtractEntities(content)
	var ids []string
	for _, e := range raw {
		id, err := addEntity(db, e.Name, e.Type, profile, e.Confidence)
		if err != nil {
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rels := 0
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if err := addRelation(db, ids[i], ids[j], "related_to", 0.5, memoryID); err != nil {
				return 0, 0, err
			}
			rels++
		}
	}
	return len(ids), rels, nil
}

// Community is a connected group of entities.
type Community struct {
	ID       string   `json:"id"`
	Members  []string `json:"member_entity_ids"`
	Label    string   `json:"label"`
	Cohesion float64  `json:"cohesion_score"`
}

func unionFind(nodes []string, edges [][2]string) map[string]string {
	parent := map[string]string{}
	for _, n := range nodes {
		parent[n] = n
	}
	find := func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for _, e := range edges {
		a, b := e[0], e[1]
		if _, oka := parent[a]; oka {
			if _, okb := parent[b]; okb {
				ra, rb := find(a), find(b)
				if ra != rb {
					parent[ra] = rb
				}
			}
		}
	}
	out := map[string]string{}
	for _, n := range nodes {
		out[n] = find(n)
	}
	return out
}

// DetectCommunities groups profile entities via union-find over in-profile relations.
func DetectCommunities(db *store.DB, profile string) ([]Community, error) {
	rows, err := db.Query(`SELECT id, name, mention_count FROM entities WHERE profile=?`, profile)
	if err != nil {
		return nil, err
	}
	var nodes []string
	nameMap := map[string]string{}
	countMap := map[string]int{}
	for rows.Next() {
		var id, name string
		var count int
		if err := rows.Scan(&id, &name, &count); err != nil {
			rows.Close()
			return nil, err
		}
		nodes = append(nodes, id)
		nameMap[id] = name
		countMap[id] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(nodes) == 0 {
		return nil, nil
	}
	rrows, err := db.Query(`SELECT r.source_entity_id, r.target_entity_id FROM relations r
		JOIN entities e1 ON r.source_entity_id=e1.id JOIN entities e2 ON r.target_entity_id=e2.id
		WHERE e1.profile=? AND e2.profile=?`, profile, profile)
	if err != nil {
		return nil, err
	}
	var edges [][2]string
	for rrows.Next() {
		var s, t string
		if err := rrows.Scan(&s, &t); err != nil {
			rrows.Close()
			return nil, err
		}
		edges = append(edges, [2]string{s, t})
	}
	if err := rrows.Err(); err != nil {
		rrows.Close()
		return nil, err
	}
	rrows.Close()

	membership := unionFind(nodes, edges)
	groups := map[string][]string{}
	// stable grouping order
	for _, id := range nodes {
		root := membership[id]
		groups[root] = append(groups[root], id)
	}
	var comms []Community
	for _, members := range groups {
		label := members[0]
		for _, m := range members {
			if countMap[m] > countMap[label] {
				label = m
			}
		}
		cohesion := float64(len(members)) / float64(len(nodes))
		if cohesion > 1.0 {
			cohesion = 1.0
		}
		comms = append(comms, Community{
			ID: uuid.NewString()[:12], Members: members,
			Label: nameMap[label], Cohesion: cohesion,
		})
	}
	sort.SliceStable(comms, func(i, j int) bool { return len(comms[i].Members) > len(comms[j].Members) })
	return comms, nil
}

// Entity is a graph node returned by queries.
type Entity struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	EntityType   string  `json:"entity_type"`
	MentionCount int     `json:"mention_count"`
	Confidence   float64 `json:"confidence"`
}

// Query returns profile entities (optionally name-filtered) ranked by mentions.
func Query(db *store.DB, profile, nameFilter string, limit int) ([]Entity, error) {
	q := `SELECT id,name,entity_type,mention_count,confidence FROM entities WHERE profile=?`
	args := []any{profile}
	if nameFilter != "" {
		q += ` AND name LIKE ? COLLATE NOCASE`
		args = append(args, "%"+nameFilter+"%")
	}
	q += ` ORDER BY mention_count DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entity
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Name, &e.EntityType, &e.MentionCount, &e.Confidence); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Relation is one edge in the entity graph (both endpoints in-profile).
type Relation struct {
	ID           string  `json:"id"`
	SourceID     string  `json:"source_entity_id"`
	TargetID     string  `json:"target_entity_id"`
	RelationType string  `json:"relation_type"`
	Confidence   float64 `json:"confidence"`
}

// Relations returns the in-profile relations touching the given entities (both
// endpoints must be entities in profile), mirroring Python query_graph's
// relations set. Pass empty entityIDs to get all in-profile relations. The
// result is always non-nil so JSON emits [] not null (parity, issue #528).
func Relations(db *store.DB, profile string, entityIDs []string) ([]Relation, error) {
	out := []Relation{}
	q := `SELECT r.id,r.source_entity_id,r.target_entity_id,r.relation_type,r.confidence
		FROM relations r
		WHERE r.source_entity_id IN (SELECT id FROM entities WHERE profile=?)
		  AND r.target_entity_id IN (SELECT id FROM entities WHERE profile=?)`
	args := []any{profile, profile}
	if len(entityIDs) > 0 {
		ph := strings.TrimRight(strings.Repeat("?,", len(entityIDs)), ",")
		q += " AND (r.source_entity_id IN (" + ph + ") OR r.target_entity_id IN (" + ph + "))"
		for _, id := range entityIDs {
			args = append(args, id)
		}
		for _, id := range entityIDs {
			args = append(args, id)
		}
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Relation
		if err := rows.Scan(&r.ID, &r.SourceID, &r.TargetID, &r.RelationType, &r.Confidence); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Summary returns (entities, relations, communities) counts for a profile.
func Summary(db *store.DB, profile string) (int, int, int, error) {
	var nEnt, nRel int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entities WHERE profile=?`, profile).Scan(&nEnt); err != nil {
		return 0, 0, 0, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM relations r
		JOIN entities e1 ON r.source_entity_id=e1.id JOIN entities e2 ON r.target_entity_id=e2.id
		WHERE e1.profile=? AND e2.profile=?`, profile, profile).Scan(&nRel); err != nil {
		return 0, 0, 0, err
	}
	comms, err := DetectCommunities(db, profile)
	if err != nil {
		return 0, 0, 0, err
	}
	return nEnt, nRel, len(comms), nil
}

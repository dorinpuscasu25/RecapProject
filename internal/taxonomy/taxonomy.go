// Package taxonomy manages database-driven categories: tree, ordering, localization, merges with
// redirects, versioning for classifier cache invalidation, and editorial controls.
package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/jobs"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/ids"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

type Category struct {
	ID                   uuid.UUID         `json:"id"`
	Slug                 string            `json:"slug"`
	ParentID             *uuid.UUID        `json:"parent_id"`
	Names                map[string]string `json:"names"`
	Descriptions         map[string]string `json:"descriptions"`
	EditorialDescription string            `json:"editorial_description"`
	IconKey              string            `json:"icon_key"`
	DisplayOrder         int               `json:"display_order"`
	Status               string            `json:"status"`
	ClassificationHints  []string          `json:"classification_hints"`
	PositiveExamples     []string          `json:"positive_examples"`
	NegativeExamples     []string          `json:"negative_examples"`
	RoutingLabels        []string          `json:"routing_labels"`
	Aliases              []string          `json:"aliases"`
	Boost                float32           `json:"boost"`
	AutoClassify         bool              `json:"auto_classify"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
	Version              int               `json:"version"`
	Children             []*Category       `json:"children,omitempty"`
}

// PublicCategory is the consumer-facing projection (no editorial internals).
type PublicCategory struct {
	ID           uuid.UUID         `json:"id"`
	Slug         string            `json:"slug"`
	ParentID     *uuid.UUID        `json:"parent_id"`
	Names        map[string]string `json:"names"`
	Descriptions map[string]string `json:"descriptions"`
	IconKey      string            `json:"icon_key"`
	DisplayOrder int               `json:"display_order"`
	Children     []*PublicCategory `json:"children,omitempty"`
}

type Service struct {
	db *db.DB

	mu        sync.RWMutex
	cachedVer int64
	cachedAll []Category
}

func NewService(d *db.DB) *Service { return &Service{db: d} }

const cols = `id, slug, parent_id, names, descriptions, editorial_description, icon_key, display_order, status,
	classification_hints, positive_examples, negative_examples, routing_labels, aliases, boost, auto_classify,
	created_at, updated_at, version`

func scan(row pgx.Row) (*Category, error) {
	var c Category
	var names, descs []byte
	err := row.Scan(&c.ID, &c.Slug, &c.ParentID, &names, &descs, &c.EditorialDescription, &c.IconKey, &c.DisplayOrder, &c.Status,
		&c.ClassificationHints, &c.PositiveExamples, &c.NegativeExamples, &c.RoutingLabels, &c.Aliases, &c.Boost, &c.AutoClassify,
		&c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(names, &c.Names)
	_ = json.Unmarshal(descs, &c.Descriptions)
	if c.Descriptions == nil {
		c.Descriptions = map[string]string{}
	}
	return &c, nil
}

// CurrentVersion returns the latest taxonomy version.
func (s *Service) CurrentVersion(ctx context.Context) (int64, error) {
	var v *int64
	err := s.db.Pool.QueryRow(ctx, `SELECT max(version) FROM taxonomy_versions`).Scan(&v)
	if v == nil {
		return 0, err
	}
	return *v, err
}

func bumpVersion(ctx context.Context, tx pgx.Tx, summary string) (int64, error) {
	var by *uuid.UUID
	if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
		by = &p.ID
	}
	var v int64
	err := tx.QueryRow(ctx, `INSERT INTO taxonomy_versions (summary, changed_by) VALUES ($1,$2) RETURNING version`, summary, by).Scan(&v)
	return v, err
}

// All returns all non-merged categories (cached per taxonomy version).
func (s *Service) All(ctx context.Context) ([]Category, int64, error) {
	ver, err := s.CurrentVersion(ctx)
	if err != nil {
		return nil, 0, err
	}
	s.mu.RLock()
	if s.cachedAll != nil && s.cachedVer == ver {
		out := s.cachedAll
		s.mu.RUnlock()
		return out, ver, nil
	}
	s.mu.RUnlock()
	rows, err := s.db.Pool.Query(ctx, `SELECT `+cols+` FROM categories WHERE status <> 'merged' ORDER BY display_order, slug`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var all []Category
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	s.cachedAll, s.cachedVer = all, ver
	s.mu.Unlock()
	return all, ver, nil
}

// Active returns categories visible to consumers and eligible for classification.
func (s *Service) Active(ctx context.Context) ([]Category, int64, error) {
	all, ver, err := s.All(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []Category
	for _, c := range all {
		if c.Status == "active" {
			out = append(out, c)
		}
	}
	return out, ver, nil
}

// Tree builds a nested tree; includeInactive controls hidden/archived inclusion (admin view).
func Tree(list []Category) []*Category {
	byID := map[uuid.UUID]*Category{}
	for i := range list {
		c := list[i]
		c.Children = nil
		byID[c.ID] = &c
	}
	var roots []*Category
	for _, c := range byID {
		if c.ParentID != nil {
			if p, ok := byID[*c.ParentID]; ok {
				p.Children = append(p.Children, c)
				continue
			}
		}
		roots = append(roots, c)
	}
	var sortRec func([]*Category)
	sortRec = func(cs []*Category) {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].DisplayOrder != cs[j].DisplayOrder {
				return cs[i].DisplayOrder < cs[j].DisplayOrder
			}
			return cs[i].Slug < cs[j].Slug
		})
		for _, c := range cs {
			sortRec(c.Children)
		}
	}
	sortRec(roots)
	return roots
}

func ToPublic(cs []*Category) []*PublicCategory {
	out := make([]*PublicCategory, 0, len(cs))
	for _, c := range cs {
		out = append(out, &PublicCategory{ID: c.ID, Slug: c.Slug, ParentID: c.ParentID, Names: c.Names, Descriptions: c.Descriptions,
			IconKey: c.IconKey, DisplayOrder: c.DisplayOrder, Children: ToPublic(c.Children)})
	}
	return out
}

// PublicTree returns active categories whose ancestors are all active.
func (s *Service) PublicTree(ctx context.Context) ([]*PublicCategory, error) {
	active, _, err := s.Active(ctx)
	if err != nil {
		return nil, err
	}
	return ToPublic(Tree(active)), nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Category, error) {
	c, err := scan(s.db.Pool.QueryRow(ctx, `SELECT `+cols+` FROM categories WHERE id=$1`, id))
	if db.IsNoRows(err) {
		return nil, apperr.NotFound("category_not_found", "Category not found.")
	}
	return c, err
}

// Resolve follows merge redirects (bounded) and returns the effective category id.
func (s *Service) Resolve(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	cur := id
	for i := 0; i < 10; i++ {
		var to uuid.UUID
		err := s.db.Pool.QueryRow(ctx, `SELECT to_category_id FROM category_redirects WHERE from_category_id=$1`, cur).Scan(&to)
		if db.IsNoRows(err) {
			return cur, nil
		}
		if err != nil {
			return uuid.Nil, err
		}
		cur = to
	}
	return cur, nil
}

// ---------- Mutations ----------

type Input struct {
	Slug                 *string           `json:"slug,omitempty"`
	ParentID             *uuid.UUID        `json:"parent_id,omitempty"`
	ClearParent          bool              `json:"clear_parent,omitempty"`
	Names                map[string]string `json:"names,omitempty"`
	Descriptions         map[string]string `json:"descriptions,omitempty"`
	EditorialDescription *string           `json:"editorial_description,omitempty"`
	IconKey              *string           `json:"icon_key,omitempty"`
	DisplayOrder         *int              `json:"display_order,omitempty"`
	Status               *string           `json:"status,omitempty"`
	ClassificationHints  []string          `json:"classification_hints,omitempty"`
	PositiveExamples     []string          `json:"positive_examples,omitempty"`
	NegativeExamples     []string          `json:"negative_examples,omitempty"`
	RoutingLabels        []string          `json:"routing_labels,omitempty"`
	Aliases              []string          `json:"aliases,omitempty"`
	Boost                *float32          `json:"boost,omitempty"`
	AutoClassify         *bool             `json:"auto_classify,omitempty"`
	Version              *int              `json:"version,omitempty"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var allowedLangs = map[string]bool{"ro": true, "ru": true, "en": true}

func validate(c *Category) error {
	var v apperr.Validator
	v.Check(slugRe.MatchString(c.Slug) && len(c.Slug) <= 80, "slug", "invalid", "Slug must be lowercase letters, digits and hyphens.")
	if strings.TrimSpace(c.Names["ro"]) == "" {
		v.Add("names.ro", "required", "A Romanian name is required.")
	}
	for k, val := range c.Names {
		if !allowedLangs[k] {
			v.Add("names."+k, "unsupported_language", "Supported languages: ro, ru, en.")
		}
		if len([]rune(val)) > 120 {
			v.Add("names."+k, "too_long", "Name must be at most 120 characters.")
		}
	}
	for k := range c.Descriptions {
		if !allowedLangs[k] {
			v.Add("descriptions."+k, "unsupported_language", "Supported languages: ro, ru, en.")
		}
	}
	switch c.Status {
	case "active", "hidden", "archived":
	default:
		v.Add("status", "invalid", "Status must be active, hidden or archived.")
	}
	v.Check(c.Boost >= -1 && c.Boost <= 1, "boost", "out_of_range", "Boost must be between -1 and 1.")
	v.Check(len(c.ClassificationHints) <= 200 && len(c.Aliases) <= 200 && len(c.PositiveExamples) <= 50 && len(c.NegativeExamples) <= 50,
		"classification_hints", "too_many", "Too many hints/aliases/examples.")
	return v.Err()
}

func apply(c *Category, in Input) {
	if in.Slug != nil {
		c.Slug = strings.ToLower(strings.TrimSpace(*in.Slug))
	}
	if in.ClearParent {
		c.ParentID = nil
	} else if in.ParentID != nil {
		c.ParentID = in.ParentID
	}
	if in.Names != nil {
		c.Names = trimMap(in.Names)
	}
	if in.Descriptions != nil {
		c.Descriptions = trimMap(in.Descriptions)
	}
	if in.EditorialDescription != nil {
		c.EditorialDescription = *in.EditorialDescription
	}
	if in.IconKey != nil {
		c.IconKey = *in.IconKey
	}
	if in.DisplayOrder != nil {
		c.DisplayOrder = *in.DisplayOrder
	}
	if in.Status != nil {
		c.Status = *in.Status
	}
	if in.ClassificationHints != nil {
		c.ClassificationHints = cleanList(in.ClassificationHints)
	}
	if in.PositiveExamples != nil {
		c.PositiveExamples = cleanList(in.PositiveExamples)
	}
	if in.NegativeExamples != nil {
		c.NegativeExamples = cleanList(in.NegativeExamples)
	}
	if in.RoutingLabels != nil {
		c.RoutingLabels = cleanList(in.RoutingLabels)
	}
	if in.Aliases != nil {
		c.Aliases = cleanList(in.Aliases)
	}
	if in.Boost != nil {
		c.Boost = *in.Boost
	}
	if in.AutoClassify != nil {
		c.AutoClassify = *in.AutoClassify
	}
}

func trimMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v = strings.TrimSpace(v); v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[strings.ToLower(s)] {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	return out
}

func actorID(ctx context.Context) *uuid.UUID {
	if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
		return &p.ID
	}
	return nil
}

func (s *Service) Create(ctx context.Context, in Input) (*Category, error) {
	c := &Category{ID: ids.New(), Names: map[string]string{}, Descriptions: map[string]string{}, Status: "active", AutoClassify: true,
		ClassificationHints: []string{}, PositiveExamples: []string{}, NegativeExamples: []string{}, RoutingLabels: []string{}, Aliases: []string{}}
	apply(c, in)
	if err := validate(c); err != nil {
		return nil, err
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if c.ParentID != nil {
			if err := checkParent(ctx, tx, c.ID, *c.ParentID); err != nil {
				return err
			}
		}
		names, _ := json.Marshal(c.Names)
		descs, _ := json.Marshal(c.Descriptions)
		_, err := tx.Exec(ctx, `INSERT INTO categories (id, slug, parent_id, names, descriptions, editorial_description, icon_key,
			display_order, status, classification_hints, positive_examples, negative_examples, routing_labels, aliases, boost, auto_classify,
			created_by, updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$17)`,
			c.ID, c.Slug, c.ParentID, names, descs, c.EditorialDescription, c.IconKey, c.DisplayOrder, c.Status,
			c.ClassificationHints, c.PositiveExamples, c.NegativeExamples, c.RoutingLabels, c.Aliases, c.Boost, c.AutoClassify, actorID(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("category_slug_taken", "A category with this slug already exists.")
		}
		if err != nil {
			return err
		}
		if _, err := bumpVersion(ctx, tx, "create "+c.Slug); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "category.create", ResourceType: "category", ResourceID: c.ID.String(), New: c})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, c.ID)
}

// checkParent verifies the parent exists, is not merged, and that setting it creates no cycle.
func checkParent(ctx context.Context, tx pgx.Tx, id, parent uuid.UUID) error {
	if id == parent {
		return apperr.ValidationMsg("category_cycle", "A category cannot be its own parent.")
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM categories WHERE id=$1`, parent).Scan(&status)
	if db.IsNoRows(err) {
		return apperr.ValidationMsg("category_parent_not_found", "Parent category does not exist.")
	}
	if err != nil {
		return err
	}
	if status == "merged" || status == "archived" {
		return apperr.ValidationMsg("category_parent_inactive", "Parent category is archived or merged.")
	}
	var cycle bool
	err = tx.QueryRow(ctx, `WITH RECURSIVE anc AS (
			SELECT id, parent_id, 1 AS depth FROM categories WHERE id=$1
			UNION ALL SELECT c.id, c.parent_id, a.depth+1 FROM categories c JOIN anc a ON c.id = a.parent_id WHERE a.depth < 50)
		SELECT EXISTS(SELECT 1 FROM anc WHERE id=$2)`, parent, id).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle {
		return apperr.ValidationMsg("category_cycle", "This parent would create a cycle in the category tree.")
	}
	var depth int
	if err := tx.QueryRow(ctx, `WITH RECURSIVE anc AS (SELECT id, parent_id FROM categories WHERE id=$1
		UNION ALL SELECT c.id, c.parent_id FROM categories c JOIN anc a ON c.id=a.parent_id) SELECT count(*) FROM anc`, parent).Scan(&depth); err != nil {
		return err
	}
	if depth >= 4 {
		return apperr.ValidationMsg("category_too_deep", "Category trees are limited to 4 levels.")
	}
	return nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (*Category, error) {
	var out *Category
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		cur, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM categories WHERE id=$1 FOR UPDATE`, id))
		if db.IsNoRows(err) {
			return apperr.NotFound("category_not_found", "Category not found.")
		}
		if err != nil {
			return err
		}
		if cur.Status == "merged" {
			return apperr.Conflict("category_merged", "Merged categories cannot be edited.")
		}
		if in.Version != nil && *in.Version != cur.Version {
			return apperr.VersionConflict("category")
		}
		old := *cur
		next := *cur
		apply(&next, in)
		if err := validate(&next); err != nil {
			return err
		}
		if next.ParentID != nil && (old.ParentID == nil || *old.ParentID != *next.ParentID) {
			if err := checkParent(ctx, tx, id, *next.ParentID); err != nil {
				return err
			}
		}
		names, _ := json.Marshal(next.Names)
		descs, _ := json.Marshal(next.Descriptions)
		_, err = tx.Exec(ctx, `UPDATE categories SET slug=$2, parent_id=$3, names=$4, descriptions=$5, editorial_description=$6, icon_key=$7,
			display_order=$8, status=$9, classification_hints=$10, positive_examples=$11, negative_examples=$12, routing_labels=$13,
			aliases=$14, boost=$15, auto_classify=$16, updated_by=$17, updated_at=now(), version=version+1 WHERE id=$1`,
			id, next.Slug, next.ParentID, names, descs, next.EditorialDescription, next.IconKey, next.DisplayOrder, next.Status,
			next.ClassificationHints, next.PositiveExamples, next.NegativeExamples, next.RoutingLabels, next.Aliases, next.Boost,
			next.AutoClassify, actorID(ctx))
		if db.IsUniqueViolation(err) {
			return apperr.Conflict("category_slug_taken", "A category with this slug already exists.")
		}
		if err != nil {
			return err
		}
		if _, err := bumpVersion(ctx, tx, "update "+next.Slug); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Action: "category.update", ResourceType: "category", ResourceID: id.String(), Old: old, New: next}); err != nil {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM categories WHERE id=$1`, id))
		return err
	})
	return out, err
}

// Reorder sets display_order for siblings in the given order.
func (s *Service) Reorder(ctx context.Context, parentID *uuid.UUID, ordered []uuid.UUID) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		for i, id := range ordered {
			tag, err := tx.Exec(ctx, `UPDATE categories SET display_order=$2, updated_at=now(), version=version+1
				WHERE id=$1 AND parent_id IS NOT DISTINCT FROM $3`, id, i*10, parentID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return apperr.ValidationMsg("category_not_sibling", fmt.Sprintf("Category %s is not a child of the given parent.", id))
			}
		}
		if _, err := bumpVersion(ctx, tx, "reorder"); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "category.reorder", ResourceType: "category", New: map[string]any{"parent_id": parentID, "order": ordered}})
	})
}

// Merge moves assignments and user subscriptions from `from` into `to`, records a redirect and
// marks `from` as merged. Children of `from` are reparented to `to`.
func (s *Service) Merge(ctx context.Context, from, to uuid.UUID, reason string) error {
	if from == to {
		return apperr.ValidationMsg("category_merge_self", "Cannot merge a category into itself.")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var fs, ts string
		if err := tx.QueryRow(ctx, `SELECT status FROM categories WHERE id=$1 FOR UPDATE`, from).Scan(&fs); err != nil {
			if db.IsNoRows(err) {
				return apperr.NotFound("category_not_found", "Source category not found.")
			}
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM categories WHERE id=$1 FOR UPDATE`, to).Scan(&ts); err != nil {
			if db.IsNoRows(err) {
				return apperr.NotFound("category_not_found", "Target category not found.")
			}
			return err
		}
		if fs == "merged" || ts == "merged" {
			return apperr.Conflict("category_merged", "One of the categories is already merged.")
		}
		// Target must not be a descendant of source (would orphan the subtree).
		var desc bool
		if err := tx.QueryRow(ctx, `WITH RECURSIVE d AS (SELECT id FROM categories WHERE parent_id=$1
			UNION ALL SELECT c.id FROM categories c JOIN d ON c.parent_id=d.id) SELECT EXISTS(SELECT 1 FROM d WHERE id=$2)`, from, to).Scan(&desc); err != nil {
			return err
		}
		if desc {
			return apperr.ValidationMsg("category_merge_descendant", "Cannot merge a category into its own descendant.")
		}
		stmts := []string{
			`INSERT INTO article_categories (article_id, category_id, source, is_primary, confidence, model_ref, taxonomy_version, assigned_by)
			 SELECT article_id, $2, source, is_primary, confidence, model_ref, taxonomy_version, assigned_by FROM article_categories WHERE category_id=$1
			 ON CONFLICT DO NOTHING`,
			`DELETE FROM article_categories WHERE category_id=$1 AND $2::uuid IS NOT NULL`,
			`INSERT INTO event_categories (event_id, category_id, source, is_primary, confidence, assigned_by)
			 SELECT event_id, $2, source, is_primary, confidence, assigned_by FROM event_categories WHERE category_id=$1 ON CONFLICT DO NOTHING`,
			`DELETE FROM event_categories WHERE category_id=$1 AND $2::uuid IS NOT NULL`,
			`UPDATE events SET primary_category_id=$2 WHERE primary_category_id=$1`,
			`INSERT INTO user_interests (id, user_id, interest_type, target_key, mode, weight, created_at)
			 SELECT gen_random_uuid(), user_id, interest_type, $2::uuid::text, mode, weight, created_at FROM user_interests
			 WHERE interest_type='category' AND target_key=$1::uuid::text ON CONFLICT DO NOTHING`,
			`DELETE FROM user_interests WHERE interest_type='category' AND target_key=$1::uuid::text AND $2::uuid IS NOT NULL`,
			`UPDATE categories SET parent_id=$2, updated_at=now(), version=version+1 WHERE parent_id=$1`,
			`UPDATE category_redirects SET to_category_id=$2 WHERE to_category_id=$1`,
		}
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q, from, to); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE categories SET status='merged', updated_at=now(), version=version+1 WHERE id=$1`, from); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO category_redirects (from_category_id, to_category_id, reason, created_by) VALUES ($1,$2,$3,$4)`,
			from, to, reason, actorID(ctx)); err != nil {
			return err
		}
		if _, err := bumpVersion(ctx, tx, "merge"); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "category.merge", ResourceType: "category", ResourceID: from.String(),
			New: map[string]any{"into": to, "reason": reason}})
	})
}

// ---------- Reclassification ----------

const JobReclassify = "taxonomy.reclassify"

type ReclassifyRequest struct {
	Since       *time.Time  `json:"since,omitempty"`
	Until       *time.Time  `json:"until,omitempty"`
	CategoryIDs []uuid.UUID `json:"category_ids,omitempty"`
	PublisherID *uuid.UUID  `json:"publisher_id,omitempty"`
	DryRun      bool        `json:"dry_run"`
	Limit       int         `json:"limit,omitempty"`
}

// RequestReclassify enqueues a reclassification job with explicit scope.
func (s *Service) RequestReclassify(ctx context.Context, r ReclassifyRequest) (uuid.UUID, error) {
	if r.Limit <= 0 || r.Limit > 50000 {
		r.Limit = 5000
	}
	var id uuid.UUID
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		id, err = jobs.Enqueue(ctx, tx, JobReclassify, r, jobs.EnqueueOpts{CreatedBy: actorID(ctx), Priority: 200})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "category.reclassify_requested", ResourceType: "job", ResourceID: id.String(), New: r})
	})
	return id, err
}

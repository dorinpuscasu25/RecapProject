package events

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dorinpuscasu25/recapproject/internal/audit"
	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/bus"
	"github.com/dorinpuscasu25/recapproject/internal/platform/db"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

func actor(ctx context.Context) *uuid.UUID {
	if p := principal.From(ctx); p != nil && p.Kind == principal.KindUser {
		return &p.ID
	}
	return nil
}

func (s *Service) bump(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind string, what map[string]any, arts []uuid.UUID) (int, error) {
	var v int
	if err := tx.QueryRow(ctx, `UPDATE events SET version=version+1, last_material_update_at=now(), updated_at=now() WHERE id=$1 RETURNING version`, id).Scan(&v); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO event_updates (id, event_id, event_version, update_kind, what_changed, supporting_article_ids, severity, approved_by, approved_at)
		VALUES ($1,$2,$3,$4,$5,$6,'major',$7, now())`, newID(), id, v, kind, mustJSON(what), nonNilUUID(arts), actor(ctx)); err != nil {
		return 0, err
	}
	if err := s.snapshot(ctx, tx, id, v, kind, nil); err != nil {
		return 0, err
	}
	_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.EventUpdated, AggregateType: "event", AggregateID: id.String(),
		Payload: Changed{EventID: id, Version: v, Kind: kind, Material: true, ArticleIDs: arts}})
	return v, err
}

func lockActive(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM events WHERE id=$1 FOR UPDATE`, id).Scan(&status)
	if db.IsNoRows(err) {
		return apperr.NotFound("event_not_found", "Event not found: "+id.String())
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return apperr.Conflict("event_not_active", "Event is not active: "+id.String())
	}
	return nil
}

// Merge moves all coverage of the source events into target. Fully reversible via Unmerge.
func (s *Service) Merge(ctx context.Context, target uuid.UUID, sources []uuid.UUID, reason string) (uuid.UUID, error) {
	if reason == "" {
		return uuid.Nil, apperr.Validation(apperr.FieldError{Field: "reason", Code: "required", Message: "A reason is required."})
	}
	histID := newID()
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLock); err != nil {
			return err
		}
		if err := lockActive(ctx, tx, target); err != nil {
			return err
		}
		moved := map[string][]uuid.UUID{}
		var all []uuid.UUID
		for _, src := range sources {
			if src == target {
				return apperr.ValidationMsg("merge_self", "Cannot merge an event into itself.")
			}
			if err := lockActive(ctx, tx, src); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `UPDATE event_article_links SET unlinked_at=now() WHERE event_id=$1 AND unlinked_at IS NULL RETURNING article_id, relation`, src)
			if err != nil {
				return err
			}
			var arts []uuid.UUID
			for rows.Next() {
				var a uuid.UUID
				var rel string
				if err := rows.Scan(&a, &rel); err != nil {
					rows.Close()
					return err
				}
				arts = append(arts, a)
			}
			rows.Close()
			for _, a := range arts {
				if _, err := tx.Exec(ctx, `INSERT INTO event_article_links (event_id, article_id, relation, confidence, rationale, linked_by)
					VALUES ($1,$2,'same_story',1,$3,$4) ON CONFLICT (event_id, article_id) DO UPDATE SET unlinked_at=NULL, relation='same_story', linked_by=EXCLUDED.linked_by`,
					target, a, mustJSON(map[string]any{"merge": histID}), "editor:"+uuidStr(actor(ctx))); err != nil {
					return err
				}
			}
			moved[src.String()] = arts
			all = append(all, arts...)
			stmts := []string{
				`INSERT INTO event_claims (event_id, claim_id) SELECT $2, claim_id FROM event_claims WHERE event_id=$1 ON CONFLICT DO NOTHING`,
				`INSERT INTO followed_events (user_id, event_id) SELECT user_id, $2 FROM followed_events WHERE event_id=$1 ON CONFLICT DO NOTHING`,
				`UPDATE claim_occurrences SET event_id=$2 WHERE event_id=$1`,
				`UPDATE events SET status='merged', merged_into=$2, updated_at=now(), version=version+1 WHERE id=$1`,
				`INSERT INTO event_relationships (from_event_id, to_event_id, relation, confidence, created_by) VALUES ($1,$2,'merged_into',1,'editor') ON CONFLICT DO NOTHING`,
			}
			for _, q := range stmts {
				if _, err := tx.Exec(ctx, q, src, target); err != nil {
					return err
				}
			}
			if err := s.requestIndex(ctx, tx, src); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_merge_history (id, operation, source_event_ids, target_event_ids, article_ids, reason, payload, performed_by)
			VALUES ($1,'merge',$2,$3,$4,$5,$6,$7)`, histID, sources, []uuid.UUID{target}, nonNilUUID(all), reason, mustJSON(map[string]any{"moved": moved}), actor(ctx)); err != nil {
			return err
		}
		if _, err := s.recompute(ctx, tx, target, true); err != nil {
			return err
		}
		if _, err := s.bump(ctx, tx, target, "merge", map[string]any{"summary": "Editor merged events", "merged": sources, "reason": reason}, all); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "event.merge", ResourceType: "event", ResourceID: target.String(),
			New: map[string]any{"sources": sources, "history_id": histID, "reason": reason}})
	})
	return histID, err
}

// Unmerge reverses a merge using the recorded history.
func (s *Service) Unmerge(ctx context.Context, historyID uuid.UUID, reason string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLock); err != nil {
			return err
		}
		var op string
		var targets []uuid.UUID
		var payload []byte
		var reverted *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT operation, target_event_ids, payload, reverted_by FROM event_merge_history WHERE id=$1 AND reverted_at IS NULL FOR UPDATE`, historyID).
			Scan(&op, &targets, &payload, &reverted)
		if db.IsNoRows(err) {
			return apperr.NotFound("merge_history_not_found", "No reversible merge with this id.")
		}
		if err != nil {
			return err
		}
		if op != "merge" {
			return apperr.ValidationMsg("not_a_merge", "Only merges can be unmerged.")
		}
		var p struct {
			Moved map[string][]uuid.UUID `json:"moved"`
		}
		_ = json.Unmarshal(payload, &p)
		target := targets[0]
		for srcStr, arts := range p.Moved {
			src, _ := uuid.Parse(srcStr)
			if _, err := tx.Exec(ctx, `UPDATE events SET status='active', merged_into=NULL, updated_at=now() WHERE id=$1`, src); err != nil {
				return err
			}
			for _, a := range arts {
				if _, err := tx.Exec(ctx, `UPDATE event_article_links SET unlinked_at=now() WHERE event_id=$1 AND article_id=$2`, target, a); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE event_article_links SET unlinked_at=NULL WHERE event_id=$1 AND article_id=$2`, src, a); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE claim_occurrences SET event_id=$1 WHERE article_id=$2`, src, a); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM event_relationships WHERE from_event_id=$1 AND to_event_id=$2 AND relation='merged_into'`, src, target); err != nil {
				return err
			}
			if _, err := s.recompute(ctx, tx, src, true); err != nil {
				return err
			}
			if _, err := s.bump(ctx, tx, src, "split", map[string]any{"summary": "Editor reverted a merge", "reason": reason}, arts); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE event_merge_history SET reverted_at=now(), reverted_by=$2 WHERE id=$1`, historyID, actor(ctx)); err != nil {
			return err
		}
		if _, err := s.recompute(ctx, tx, target, true); err != nil {
			return err
		}
		if _, err := s.bump(ctx, tx, target, "split", map[string]any{"summary": "Editor reverted a merge", "reason": reason}, nil); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "event.unmerge", ResourceType: "event", ResourceID: target.String(), New: map[string]any{"history_id": historyID, "reason": reason}})
	})
}

// Split moves the given articles out of an event into a new event.
func (s *Service) Split(ctx context.Context, eventID uuid.UUID, articles []uuid.UUID, reason string) (uuid.UUID, error) {
	if len(articles) == 0 || reason == "" {
		return uuid.Nil, apperr.ValidationMsg("split_invalid", "article_ids and reason are required.")
	}
	var newEvent uuid.UUID
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLock); err != nil {
			return err
		}
		if err := lockActive(ctx, tx, eventID); err != nil {
			return err
		}
		var remaining int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_article_links WHERE event_id=$1 AND unlinked_at IS NULL AND NOT (article_id = ANY($2))`, eventID, articles).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return apperr.ValidationMsg("split_all", "At least one article must remain in the original event.")
		}
		tag, err := tx.Exec(ctx, `UPDATE event_article_links SET unlinked_at=now() WHERE event_id=$1 AND article_id=ANY($2) AND unlinked_at IS NULL`, eventID, articles)
		if err != nil {
			return err
		}
		if int(tag.RowsAffected()) != len(articles) {
			return apperr.ValidationMsg("split_not_linked", "All article_ids must be linked to the event.")
		}
		first, err := s.loadArticle(ctx, tx, articles[0])
		if err != nil {
			return err
		}
		newEvent, err = s.createEvent(ctx, tx, first, &decision{Relation: "new_event", Method: "editor", Reason: "split: " + reason})
		if err != nil {
			return err
		}
		for _, a := range articles[1:] {
			if _, err := tx.Exec(ctx, `INSERT INTO event_article_links (event_id, article_id, relation, confidence, linked_by) VALUES ($1,$2,'same_story',1,$3)`,
				newEvent, a, "editor:"+uuidStr(actor(ctx))); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE claim_occurrences SET event_id=$1 WHERE article_id=$2`, newEvent, a); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_relationships (from_event_id, to_event_id, relation, confidence, created_by) VALUES ($1,$2,'split_from',1,'editor')`, newEvent, eventID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_merge_history (id, operation, source_event_ids, target_event_ids, article_ids, reason, performed_by)
			VALUES ($1,'split',$2,$3,$4,$5,$6)`, newID(), []uuid.UUID{eventID}, []uuid.UUID{newEvent}, articles, reason, actor(ctx)); err != nil {
			return err
		}
		if _, err := s.recompute(ctx, tx, newEvent, true); err != nil {
			return err
		}
		if _, err := s.recompute(ctx, tx, eventID, true); err != nil {
			return err
		}
		if _, err := s.bump(ctx, tx, eventID, "split", map[string]any{"summary": "Editor split coverage into a new event", "new_event_id": newEvent, "reason": reason}, articles); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "event.split", ResourceType: "event", ResourceID: eventID.String(),
			New: map[string]any{"new_event_id": newEvent, "article_ids": articles, "reason": reason}})
	})
	return newEvent, err
}

type CorrectInput struct {
	Titles            map[string]string `json:"titles,omitempty"`
	State             *string           `json:"state,omitempty"`
	Lock              *bool             `json:"lock_summaries,omitempty"`
	CategoryIDs       []uuid.UUID       `json:"category_ids,omitempty"`
	PrimaryCategoryID *uuid.UUID        `json:"primary_category_id,omitempty"`
	Note              string            `json:"note"`
	Version           *int              `json:"version,omitempty"`
}

// Correct applies an editorial correction with a visible timeline entry and version history.
func (s *Service) Correct(ctx context.Context, id uuid.UUID, in CorrectInput) (*Event, error) {
	if in.Note == "" {
		return nil, apperr.Validation(apperr.FieldError{Field: "note", Code: "required", Message: "A correction note is required."})
	}
	if in.State != nil {
		switch *in.State {
		case "developing", "ongoing", "resolved", "corrected", "disputed":
		default:
			return nil, apperr.ValidationMsg("event_state_invalid", "Invalid state.")
		}
	}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var titlesRaw []byte
		var version int
		var state string
		if err := tx.QueryRow(ctx, `SELECT titles, version, state FROM events WHERE id=$1 AND status='active' FOR UPDATE`, id).Scan(&titlesRaw, &version, &state); err != nil {
			if db.IsNoRows(err) {
				return apperr.NotFound("event_not_found", "Event not found.")
			}
			return err
		}
		if in.Version != nil && *in.Version != version {
			return apperr.VersionConflict("event")
		}
		titles := map[string]string{}
		_ = json.Unmarshal(titlesRaw, &titles)
		for k, v := range in.Titles {
			if k == "ro" || k == "ru" || k == "en" {
				titles[k] = v
			}
		}
		newState := state
		if in.State != nil {
			newState = *in.State
		}
		editorial := "reviewed"
		if in.Lock != nil && *in.Lock {
			editorial = "locked"
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET titles=$2, state=$3, editorial_status=$4 WHERE id=$1`, id, mustJSON(titles), newState, editorial); err != nil {
			return err
		}
		if in.CategoryIDs != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM event_categories WHERE event_id=$1 AND source='editorial'`, id); err != nil {
				return err
			}
			for _, c := range in.CategoryIDs {
				prim := in.PrimaryCategoryID != nil && *in.PrimaryCategoryID == c
				if _, err := tx.Exec(ctx, `INSERT INTO event_categories (event_id, category_id, source, is_primary, assigned_by) VALUES ($1,$2,'editorial',$3,$4)`,
					id, c, prim, actor(ctx)); err != nil {
					if db.IsForeignKeyViolation(err) {
						return apperr.ValidationMsg("category_not_found", "Unknown category id.")
					}
					return err
				}
			}
			if in.PrimaryCategoryID != nil {
				if _, err := tx.Exec(ctx, `UPDATE event_categories SET is_primary=false WHERE event_id=$1 AND source='derived'`, id); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE events SET primary_category_id=$2 WHERE id=$1`, id, *in.PrimaryCategoryID); err != nil {
					return err
				}
			}
		}
		if _, err := s.bump(ctx, tx, id, "correction", map[string]any{"summary": in.Note, "editorial": true}, nil); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "event.correct", ResourceType: "event", ResourceID: id.String(),
			Old: map[string]any{"titles": json.RawMessage(titlesRaw), "state": state}, New: in})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id, "ro")
}

// ApproveUpdate makes a held timeline entry (e.g. contradiction) visible to readers.
func (s *Service) ApproveUpdate(ctx context.Context, eventID, updateID uuid.UUID) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE event_updates SET approved_by=$3, approved_at=now() WHERE id=$1 AND event_id=$2 AND requires_approval AND approved_at IS NULL`,
			updateID, eventID, actor(ctx))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.NotFound("update_not_found", "No pending update with this id.")
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "event.update_approved", ResourceType: "event", ResourceID: eventID.String(), New: map[string]any{"update_id": updateID}})
	})
}

// Versions lists the event's version history.
func (s *Service) Versions(ctx context.Context, id uuid.UUID) ([]map[string]any, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT version, change_reason, created_by, model_ref, created_at, snapshot FROM event_versions WHERE event_id=$1 ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var v int
		var reason string
		var by *uuid.UUID
		var model *string
		var at any
		var snap json.RawMessage
		if err := rows.Scan(&v, &reason, &by, &model, &at, &snap); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"version": v, "change_reason": reason, "created_by": by, "model_ref": model, "created_at": at, "snapshot": snap})
	}
	return out, rows.Err()
}

func (s *Service) requestIndex(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := bus.Publish(ctx, tx, bus.Draft{Type: bus.SearchIndexRequested, AggregateType: "event", AggregateID: id.String(),
		Payload: map[string]any{"doc_type": "event", "id": id, "force": true}})
	return err
}

func uuidStr(u *uuid.UUID) string {
	if u == nil {
		return "system"
	}
	return u.String()
}

// Refresh recomputes an event after coverage was removed (withdrawal/quarantine). Events left
// without coverage are withdrawn.
func (s *Service) Refresh(ctx context.Context, id uuid.UUID, note string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockActive(ctx, tx, id); err != nil {
			if apperr.Is(err, "event_not_active") {
				return nil
			}
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_article_links WHERE event_id=$1 AND unlinked_at IS NULL`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.Exec(ctx, `UPDATE events SET status='withdrawn', version=version+1, updated_at=now() WHERE id=$1`, id); err != nil {
				return err
			}
			return s.requestIndex(ctx, tx, id)
		}
		if _, err := s.recompute(ctx, tx, id, true); err != nil {
			return err
		}
		_, err := s.bump(ctx, tx, id, "correction", map[string]any{"summary": note}, nil)
		return err
	})
}

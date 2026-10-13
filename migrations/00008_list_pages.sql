-- +goose Up
-- HTML list pages: selectors that locate article links on a section/list page.
ALTER TABLE extraction_profiles ADD COLUMN link_selectors text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE extraction_profiles DROP COLUMN IF EXISTS link_selectors;

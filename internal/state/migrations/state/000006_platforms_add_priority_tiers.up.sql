ALTER TABLE platforms
ADD COLUMN priority_tiers_json TEXT NOT NULL DEFAULT '[]';

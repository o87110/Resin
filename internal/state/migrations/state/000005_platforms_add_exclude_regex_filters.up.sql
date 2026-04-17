ALTER TABLE platforms
ADD COLUMN exclude_regex_filters_json TEXT NOT NULL DEFAULT '[]';

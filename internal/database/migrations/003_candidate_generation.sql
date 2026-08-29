ALTER TABLE candidates ADD COLUMN generation INTEGER NOT NULL DEFAULT 1 CHECK (
    typeof(generation) = 'integer' AND generation > 0
);

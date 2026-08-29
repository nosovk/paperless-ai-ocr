CREATE TABLE candidates (
    document_id INTEGER PRIMARY KEY CHECK (document_id > 0),
    priority INTEGER NOT NULL CHECK (
        typeof(priority) = 'integer' AND priority IN (0, 100)
    ),
    generation INTEGER NOT NULL CHECK (
        typeof(generation) = 'integer' AND generation > 0
    ),
    created_at TEXT NOT NULL CHECK (length(created_at) > 0),
    updated_at TEXT NOT NULL CHECK (length(updated_at) > 0)
) STRICT;

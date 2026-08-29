CREATE TABLE jobs (
    id INTEGER PRIMARY KEY CHECK (id > 0),
    document_id INTEGER NOT NULL CHECK (document_id > 0),
    source_checksum TEXT NOT NULL CHECK (length(source_checksum) > 0),
    priority INTEGER NOT NULL DEFAULT 0 CHECK (typeof(priority) = 'integer'),
    state TEXT NOT NULL CHECK (
        state IN ('pending', 'processing', 'retry', 'completed', 'failed')
    ),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TEXT NOT NULL CHECK (length(available_at) > 0),
    lease_owner TEXT,
    lease_expires_at TEXT,
    model TEXT NOT NULL CHECK (length(model) > 0),
    prompt_version TEXT NOT NULL CHECK (length(prompt_version) > 0),
    error_category TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL CHECK (length(created_at) > 0),
    updated_at TEXT NOT NULL CHECK (length(updated_at) > 0),
    completed_at TEXT,
    CHECK (
        (lease_owner IS NULL AND lease_expires_at IS NULL)
        OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),
    CHECK (
        (error_category IS NULL AND error_message IS NULL)
        OR (error_category IS NOT NULL AND error_message IS NOT NULL)
    )
) STRICT;

CREATE UNIQUE INDEX jobs_one_current_document_source
ON jobs (document_id, source_checksum)
WHERE state IN ('pending', 'processing', 'retry');

CREATE INDEX jobs_claim_order
ON jobs (state, available_at, priority DESC, created_at, id);

CREATE TABLE batches (
    id INTEGER PRIMARY KEY CHECK (id > 0),
    job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    page_start INTEGER NOT NULL CHECK (page_start > 0),
    page_end INTEGER NOT NULL CHECK (page_end >= page_start),
    render_dpi INTEGER NOT NULL CHECK (render_dpi > 0),
    render_format TEXT NOT NULL CHECK (length(render_format) > 0),
    state TEXT NOT NULL CHECK (
        state IN ('pending', 'processing', 'retry', 'completed', 'failed')
    ),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TEXT NOT NULL CHECK (length(available_at) > 0),
    lease_owner TEXT,
    lease_expires_at TEXT,
    result_text TEXT,
    error_category TEXT,
    error_message TEXT,
    created_at TEXT NOT NULL CHECK (length(created_at) > 0),
    updated_at TEXT NOT NULL CHECK (length(updated_at) > 0),
    completed_at TEXT,
    UNIQUE (job_id, page_start, page_end, render_dpi, render_format),
    CHECK (
        (lease_owner IS NULL AND lease_expires_at IS NULL)
        OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),
    CHECK (
        (error_category IS NULL AND error_message IS NULL)
        OR (error_category IS NOT NULL AND error_message IS NOT NULL)
    ),
    CHECK (
        state != 'completed'
        OR (result_text IS NOT NULL AND length(result_text) > 0)
    )
) STRICT;

CREATE INDEX batches_job_state_pages
ON batches (job_id, state, page_start, page_end);

CREATE TABLE settings (
    key TEXT PRIMARY KEY CHECK (length(key) > 0),
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL CHECK (length(updated_at) > 0)
) STRICT;

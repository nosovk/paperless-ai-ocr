CREATE TABLE finalizations (
    job_id INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    stage TEXT NOT NULL CHECK (
        stage IN (
            'pending',
            'content_updated',
            'complete_tag_added',
            'failed_tag_removed',
            'metadata_dispatched',
            'failure_pending',
            'failure_tag_added'
        )
    ),
    created_at TEXT NOT NULL CHECK (length(created_at) > 0),
    updated_at TEXT NOT NULL CHECK (length(updated_at) > 0)
) STRICT;

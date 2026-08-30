CREATE TABLE finalization_controls (
    job_id INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    admission_token TEXT,
    admission_attempt INTEGER CHECK (
        admission_attempt IS NULL OR (typeof(admission_attempt) = 'integer' AND admission_attempt > 0)
    ),
    admission_owner TEXT,
    admission_expires_at TEXT,
    dispatch_state TEXT NOT NULL DEFAULT 'none' CHECK (
        dispatch_state IN ('none', 'reserved', 'confirmed')
    ),
    failure_category TEXT,
    failure_message TEXT,
    created_at TEXT NOT NULL CHECK (length(created_at) > 0),
    updated_at TEXT NOT NULL CHECK (length(updated_at) > 0),
    CHECK (
        (admission_token IS NULL AND admission_attempt IS NULL AND admission_owner IS NULL AND admission_expires_at IS NULL)
        OR (admission_token IS NOT NULL AND length(admission_token) > 0
            AND admission_attempt IS NOT NULL AND admission_owner IS NOT NULL
            AND length(admission_owner) > 0 AND admission_expires_at IS NOT NULL)
    ),
    CHECK (
        (failure_category IS NULL AND failure_message IS NULL)
        OR (failure_category IS NOT NULL AND failure_message IS NOT NULL AND length(failure_message) > 0)
    )
) STRICT;

-- One row per check of a phone number. Rows are never deleted: R-16 and R-19
-- keep every result. All times are UTC timestamptz (TC-24).
-- Reversal: DROP TABLE checks. Not automated, because it destroys the check
-- history that R-16 requires; an operator runs it by hand if ever needed.
CREATE TABLE IF NOT EXISTS checks (
    id             bigserial   PRIMARY KEY,
    number         text        NOT NULL,
    state          text        NOT NULL CHECK (state IN ('in_progress', 'done')),
    result_code    text        NOT NULL DEFAULT '',
    reason         text        NOT NULL DEFAULT '',
    text           text        NOT NULL DEFAULT '',
    recording_path text        NOT NULL DEFAULT '',
    requested_at   timestamptz NOT NULL,
    started_at     timestamptz,
    finished_at    timestamptz
);

CREATE INDEX IF NOT EXISTS checks_number_started_at_idx ON checks (number, started_at);

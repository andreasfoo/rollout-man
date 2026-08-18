// Package store is the PostgreSQL read model. It is not the source of truth
// for orchestration, but it is the only thing that survives history retention,
// so every terminal fact lands here.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	_ "github.com/lib/pq"
)

type DB struct{ *sql.DB }

func Open(ctx context.Context, dsn string) (*DB, error) {
	d, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := d.PingContext(ctx); err != nil {
		return nil, err
	}
	db := &DB{d}
	return db, db.migrate(ctx)
}

const schema = `
CREATE TABLE IF NOT EXISTS case_versions (
  sha256        TEXT PRIMARY KEY,
  case_path     TEXT NOT NULL,
  source        JSONB NOT NULL,
  pinned_ref    TEXT,
  size_bytes    BIGINT,
  state         TEXT NOT NULL CHECK (state IN ('RESOLVING','READY','ADMITTED','REJECTED','INVALID')),
  error         TEXT,
  admission_result JSONB,
  admitted_at   TIMESTAMPTZ,
  task_config   JSONB,
  first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS llm_specs (
  name           TEXT PRIMARY KEY,
  provider       TEXT,
  base_url       TEXT,
  model          TEXT,
  api_key_env    TEXT,
  api_key_cmd    JSONB,
  max_concurrent INT,
  parameters     JSONB,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS experiments (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  config       JSONB NOT NULL,
  state        TEXT NOT NULL,
  created_by   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  confirmed_at TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS tasks (
  id            TEXT PRIMARY KEY,
  experiment_id TEXT NOT NULL REFERENCES experiments(id),
  case_sha256   TEXT NOT NULL,
  case_path     TEXT NOT NULL,
  agent         TEXT NOT NULL,
  llm_spec      TEXT,
  state         TEXT NOT NULL CHECK (state IN ('PENDING','RUNNING','COMPLETED','CANCELLED')),
  requested_trials INT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS trials (
  id            TEXT PRIMARY KEY,
  task_id       TEXT NOT NULL REFERENCES tasks(id),
  trial_index   INT NOT NULL,
  attempt       INT NOT NULL,
  runner_id     TEXT,
  state         TEXT NOT NULL CHECK (state IN ('PENDING','RUNNING','COMPLETED','FAILED','CANCELLED')),
  reward        DOUBLE PRECISION,
  failure_category TEXT,
  failure_code  TEXT,
  failure_message  TEXT,
  metrics       JSONB,
  first_queued_at  TIMESTAMPTZ,
  started_at    TIMESTAMPTZ,
  finished_at   TIMESTAMPTZ,
  UNIQUE (task_id, trial_index, attempt)
);

CREATE TABLE IF NOT EXISTS artifacts (
  id          BIGSERIAL PRIMARY KEY,
  trial_id    TEXT NOT NULL REFERENCES trials(id),
  attempt     INT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('bundle','traj','log','result')),
  name        TEXT NOT NULL,
  object_key  TEXT NOT NULL,
  size_bytes  BIGINT,
  sha256      TEXT,
  redaction   JSONB,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (trial_id, attempt, name)
);

CREATE TABLE IF NOT EXISTS events (
  id         BIGSERIAL PRIMARY KEY,
  experiment_id TEXT,
  trial_id   TEXT,
  type       TEXT NOT NULL,
  ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
  payload    JSONB
);
CREATE INDEX IF NOT EXISTS events_trial_ts ON events (trial_id, ts);
`

func (db *DB) migrate(ctx context.Context) error {
	_, err := db.ExecContext(ctx, schema)
	return err
}

func js(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (db *DB) UpsertCaseVersion(ctx context.Context, sha, path string, source any, pinned string, state, errMsg string, cfg any, size int64) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO case_versions (sha256, case_path, source, pinned_ref, size_bytes, state, error, task_config)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)
ON CONFLICT (sha256) DO UPDATE SET
  case_path=EXCLUDED.case_path, source=EXCLUDED.source, pinned_ref=EXCLUDED.pinned_ref,
  size_bytes=EXCLUDED.size_bytes, state=EXCLUDED.state, error=EXCLUDED.error,
  task_config=EXCLUDED.task_config`,
		sha, path, js(source), pinned, size, state, errMsg, js(cfg))
	return err
}

func (db *DB) SetAdmission(ctx context.Context, sha, state string, result any) error {
	var at any
	if state == "ADMITTED" {
		at = time.Now()
	}
	_, err := db.ExecContext(ctx,
		`UPDATE case_versions SET state=$2, admission_result=$3, admitted_at=$4 WHERE sha256=$1`,
		sha, state, js(result), at)
	return err
}

func (db *DB) UpsertLLMSpec(ctx context.Context, name, provider, baseURL, model, keyEnv string, keyCmd []string, maxConc int, params any) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO llm_specs (name, provider, base_url, model, api_key_env, api_key_cmd, max_concurrent, parameters, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
ON CONFLICT (name) DO UPDATE SET
  provider=EXCLUDED.provider, base_url=EXCLUDED.base_url, model=EXCLUDED.model,
  api_key_env=EXCLUDED.api_key_env, api_key_cmd=EXCLUDED.api_key_cmd,
  max_concurrent=EXCLUDED.max_concurrent, parameters=EXCLUDED.parameters, updated_at=now()`,
		name, provider, baseURL, model, keyEnv, js(keyCmd), maxConc, js(params))
	return err
}

func (db *DB) CreateExperiment(ctx context.Context, id, name string, cfg any) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO experiments (id,name,config,state,confirmed_at) VALUES ($1,$2,$3,'RUNNING',now())
		 ON CONFLICT (id) DO NOTHING`, id, name, js(cfg))
	return err
}

func (db *DB) FinishExperiment(ctx context.Context, id, state string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE experiments SET state=$2, finished_at=now() WHERE id=$1`, id, state)
	return err
}

func (db *DB) CreateTask(ctx context.Context, id, expID, sha, path, agent, llmSpec string, trials int) error {
	var spec any
	if llmSpec != "" {
		spec = llmSpec
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO tasks (id,experiment_id,case_sha256,case_path,agent,llm_spec,state,requested_trials)
VALUES ($1,$2,$3,$4,$5,$6,'PENDING',$7) ON CONFLICT (id) DO NOTHING`,
		id, expID, sha, path, agent, spec, trials)
	return err
}

func (db *DB) SetTaskState(ctx context.Context, id, state string) error {
	_, err := db.ExecContext(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, id, state)
	return err
}

func (db *DB) StartTrial(ctx context.Context, id, taskID string, idx, attempt int, runner string) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO trials (id,task_id,trial_index,attempt,runner_id,state,first_queued_at,started_at)
VALUES ($1,$2,$3,$4,$5,'RUNNING',now(),now())
ON CONFLICT (id) DO UPDATE SET state='RUNNING', attempt=EXCLUDED.attempt, started_at=now()`,
		id, taskID, idx, attempt, runner)
	return err
}

func (db *DB) CompleteTrial(ctx context.Context, id string, reward float64, metrics any) error {
	_, err := db.ExecContext(ctx, `
UPDATE trials SET state='COMPLETED', reward=$2, metrics=$3, finished_at=now() WHERE id=$1`,
		id, reward, js(metrics))
	return err
}

func (db *DB) FailTrial(ctx context.Context, id, category, code, msg string, metrics any) error {
	_, err := db.ExecContext(ctx, `
UPDATE trials SET state='FAILED', failure_category=$2, failure_code=$3, failure_message=$4,
                  metrics=$5, finished_at=now() WHERE id=$1`,
		id, category, code, msg, js(metrics))
	return err
}

func (db *DB) AddArtifact(ctx context.Context, trialID string, attempt int, kind, name, key string, size int64, sha string, redaction any) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO artifacts (trial_id,attempt,kind,name,object_key,size_bytes,sha256,redaction)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (trial_id,attempt,name) DO UPDATE SET
  object_key=EXCLUDED.object_key, size_bytes=EXCLUDED.size_bytes,
  sha256=EXCLUDED.sha256, redaction=EXCLUDED.redaction`,
		trialID, attempt, kind, name, key, size, sha, js(redaction))
	return err
}

func (db *DB) Event(ctx context.Context, expID, trialID, typ string, payload any) error {
	var e, t any
	if expID != "" {
		e = expID
	}
	if trialID != "" {
		t = trialID
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO events (experiment_id,trial_id,type,payload) VALUES ($1,$2,$3,$4)`,
		e, t, typ, js(payload))
	return err
}

type Row struct {
	Agent, LLMSpec, CasePath string
	Completed                int
	Rewards                  []float64
	FailCounts               map[string]int
}

func (db *DB) Results(ctx context.Context, expID string) ([]Row, error) {
	rows, err := db.QueryContext(ctx, `
SELECT t.agent, COALESCE(t.llm_spec,''), t.case_path, tr.state, tr.reward, COALESCE(tr.failure_code,'')
FROM trials tr JOIN tasks t ON t.id = tr.task_id
WHERE t.experiment_id = $1
ORDER BY t.agent, t.llm_spec, t.case_path`, expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	idx := map[string]*Row{}
	var out []*Row
	for rows.Next() {
		var agent, spec, path, state, code string
		var reward sql.NullFloat64
		if err := rows.Scan(&agent, &spec, &path, &state, &reward, &code); err != nil {
			return nil, err
		}
		k := agent + "|" + spec
		r, ok := idx[k]
		if !ok {
			r = &Row{Agent: agent, LLMSpec: spec, FailCounts: map[string]int{}}
			idx[k] = r
			out = append(out, r)
		}
		switch state {
		case "COMPLETED":
			r.Completed++
			if reward.Valid {
				r.Rewards = append(r.Rewards, reward.Float64)
			}
		default:
			if code == "" {
				code = "UNKNOWN"
			}
			r.FailCounts[code]++
		}
	}
	res := make([]Row, 0, len(out))
	for _, r := range out {
		res = append(res, *r)
	}
	return res, rows.Err()
}

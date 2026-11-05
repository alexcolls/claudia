package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// IDs of credentials, agents and tasks are never reused. A task ID names a git branch
// and a work directory, and an agent ID owns a volume and a cost history, so a reused
// ID would inherit a deleted one's branch or spend.
const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS credentials (
	id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL, preset TEXT NOT NULL,
	secret BLOB NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agents (
	id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL, cli TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL, allowed_tools TEXT NOT NULL DEFAULT '', credential_id INTEGER,
	accepts_queue INTEGER NOT NULL DEFAULT 1, token TEXT NOT NULL,
	max_run_usd REAL NOT NULL DEFAULT 0, daily_budget_usd REAL NOT NULL DEFAULT 0,
	run_limit_sec INTEGER NOT NULL DEFAULT 3600, pause_reason TEXT NOT NULL DEFAULT '',
	session TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, account TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, marks TEXT NOT NULL DEFAULT '{}');
CREATE TABLE IF NOT EXISTS tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, repo TEXT NOT NULL, base_branch TEXT NOT NULL DEFAULT 'main',
	prompt TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0, cli TEXT NOT NULL DEFAULT '', preset TEXT NOT NULL DEFAULT '',
	git_credential_id INTEGER, status TEXT NOT NULL DEFAULT 'queued', agent_id INTEGER,
	attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 2,
	session TEXT NOT NULL DEFAULT '', start_sha TEXT NOT NULL DEFAULT '', head_sha TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS task_deps (task_id INTEGER NOT NULL, dep_id INTEGER NOT NULL, PRIMARY KEY (task_id, dep_id));
CREATE TABLE IF NOT EXISTS runs (
	id INTEGER PRIMARY KEY, agent_id INTEGER NOT NULL, agent_name TEXT NOT NULL DEFAULT '', preset TEXT NOT NULL DEFAULT '',
	task_id INTEGER, session TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'running', error TEXT NOT NULL DEFAULT '', usd REAL NOT NULL DEFAULT 0,
	usd_state TEXT NOT NULL DEFAULT '', input INTEGER NOT NULL DEFAULT 0, output INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0, turns INTEGER NOT NULL DEFAULT 0,
	api_ms INTEGER NOT NULL DEFAULT 0, retries INTEGER NOT NULL DEFAULT 0, tool_calls INTEGER NOT NULL DEFAULT 0,
	denials INTEGER NOT NULL DEFAULT 0, exit_code INTEGER NOT NULL DEFAULT 0, stderr_tail TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS runs_started ON runs(started_at);
CREATE TABLE IF NOT EXISTS run_models (
	run_id INTEGER NOT NULL, model TEXT NOT NULL, input INTEGER NOT NULL, output INTEGER NOT NULL,
	cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL, usd REAL NOT NULL, PRIMARY KEY (run_id, model));
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT, agent_id INTEGER NOT NULL, run_id INTEGER NOT NULL,
	ts INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_agent ON events(agent_id, id);
CREATE TABLE IF NOT EXISTS approvals (
	id INTEGER PRIMARY KEY, agent_id INTEGER NOT NULL, tool TEXT NOT NULL, input TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending', message TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL, decided_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS prices (
	model TEXT PRIMARY KEY, input REAL NOT NULL, output REAL NOT NULL, cache_read REAL NOT NULL,
	cache_write REAL NOT NULL, checked_at TEXT NOT NULL);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// ponytail: one connection serialises all DB access. Enough for one process
	// and a few dozen agents; never hold rows open across another query.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Databases from v0.3 don't have this column.
	if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN account TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return db, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
func now() int64        { return time.Now().Unix() }

func (a *App) setting(key string) string {
	var v string
	a.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (a *App) setSetting(key, value string) error {
	_, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (a *App) settingFloat(key string) float64 {
	f, _ := strconv.ParseFloat(a.setting(key), 64)
	return f
}

// ---- secrets

// loadKey reads the AES key from a 0600 file, creating it on first start.
func loadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is not a 32-byte hex key", path)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600)
}

func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func unseal(key, box []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(box) < gcm.NonceSize() {
		return nil, errors.New("sealed value is too short")
	}
	return gcm.Open(nil, box[:gcm.NonceSize()], box[gcm.NonceSize():], nil)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS has no randomness left: nothing here can be safe
	}
	return hex.EncodeToString(b)
}

// ---- credentials

// Preset fills in a credential form. Variable names come from each provider's docs.
type Preset struct {
	Key   string            `json:"key"`
	Label string            `json:"label"`
	CLI   string            `json:"cli"` // empty for git tokens
	Note  string            `json:"note,omitempty"`
	Env   []string          `json:"env"`
	Files []string          `json:"files,omitempty"` // env vars that hold a path to a secret file
	Fixed map[string]string `json:"fixed,omitempty"`
}

var presets = []Preset{
	{Key: "anthropic", Label: "Anthropic API", CLI: "claude", Env: []string{"ANTHROPIC_API_KEY"}},
	{Key: "bedrock", Label: "Amazon Bedrock", CLI: "claude", Env: []string{"AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"},
		Fixed: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}},
	{Key: "vertex", Label: "Google Cloud", CLI: "claude", Env: []string{"CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID"},
		Files: []string{"GOOGLE_APPLICATION_CREDENTIALS"}, Fixed: map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}},
	{Key: "foundry", Label: "Microsoft Foundry", CLI: "claude", Env: []string{"ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_FOUNDRY_API_KEY"},
		Fixed: map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1"}},
	{Key: "gateway", Label: "Anthropic-format gateway (Claude models)", CLI: "claude", Env: []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"}},
	{Key: "gateway_other", Label: "Gateway with non-Claude models", CLI: "claude", Env: []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"},
		Note: "Anthropic doesn't support Claude Code on non-Claude models. Tool use and cost figures can break."},
	{Key: "openai", Label: "OpenAI API", CLI: "codex", Env: []string{"CODEX_API_KEY"}},
	{Key: "gemini", Label: "Gemini API key", CLI: "agy", Env: []string{"GEMINI_API_KEY"}, Note: "Experimental."},
	{Key: "gcp_adc", Label: "Google Application Default Credentials", CLI: "agy", Env: []string{"GOOGLE_CLOUD_QUOTA_PROJECT", "GOOGLE_CLOUD_LOCATION"},
		Files: []string{"GOOGLE_APPLICATION_CREDENTIALS"}, Fixed: map[string]string{"AGY_ADC_AUTH": "true"}, Note: "Experimental."},
	{Key: "git", Label: "Git token (fine-grained, one repo)", Env: []string{"GIT_TOKEN"}},
}

func presetByKey(key string) (Preset, bool) {
	for _, p := range presets {
		if p.Key == key {
			return p, true
		}
	}
	return Preset{}, false
}

// Bundle is a credential's secret contents.
type Bundle struct {
	Env   map[string]string `json:"env"`
	Files map[string]string `json:"files,omitempty"` // env var name to file contents
}

var envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// validateBundle rejects Claude subscription tokens. Anthropic's terms bar
// collecting or storing them: sign-in happens in the agent's terminal instead.
func validateBundle(b Bundle) error {
	for k, v := range b.Env {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("%q isn't a valid variable name", k)
		}
		if k == "CLAUDE_CODE_OAUTH_TOKEN" || strings.HasPrefix(v, "sk-ant-oat") {
			return errors.New("Claude subscription tokens can't be stored here. Sign in from the agent's terminal tab instead")
		}
	}
	for k := range b.Files {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("%q isn't a valid variable name", k)
		}
	}
	return nil
}

// Credential is the public view: values never leave the server.
type Credential struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Preset    string   `json:"preset"`
	Vars      []string `json:"vars"`
	CreatedAt int64    `json:"created_at"`
}

func (a *App) addCredential(name, preset string, b Bundle) (int64, error) {
	p, ok := presetByKey(preset)
	if !ok {
		return 0, fmt.Errorf("unknown preset %q", preset)
	}
	if strings.TrimSpace(name) == "" {
		return 0, errors.New("a name is required")
	}
	if err := validateBundle(b); err != nil {
		return 0, err
	}
	if b.Env == nil {
		b.Env = map[string]string{}
	}
	for k, v := range p.Fixed {
		b.Env[k] = v
	}
	plain, _ := json.Marshal(b)
	box, err := seal(a.key, plain)
	if err != nil {
		return 0, err
	}
	res, err := a.db.Exec(`INSERT INTO credentials(name,preset,secret,created_at) VALUES(?,?,?,?)`, name, preset, box, now())
	if err != nil {
		return 0, fmt.Errorf("couldn't save credential: %w", err)
	}
	return res.LastInsertId()
}

func (a *App) bundle(id int64) (Bundle, error) {
	var b Bundle
	if id == 0 {
		return b, nil
	}
	var box []byte
	if err := a.db.QueryRow(`SELECT secret FROM credentials WHERE id=?`, id).Scan(&box); err != nil {
		return b, fmt.Errorf("credential %d: %w", id, err)
	}
	plain, err := unseal(a.key, box)
	if err != nil {
		return b, fmt.Errorf("credential %d can't be decrypted: %w", id, err)
	}
	return b, json.Unmarshal(plain, &b)
}

func (a *App) credentials() ([]Credential, error) {
	rows, err := a.db.Query(`SELECT id,name,preset,secret,created_at FROM credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		var c Credential
		var box []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.Preset, &box, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Vars = []string{}
		if plain, err := unseal(a.key, box); err == nil {
			var b Bundle
			if json.Unmarshal(plain, &b) == nil {
				for k := range b.Env {
					c.Vars = append(c.Vars, k)
				}
				for k := range b.Files {
					c.Vars = append(c.Vars, k)
				}
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- agents

type Agent struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	CLI            string  `json:"cli"`
	Model          string  `json:"model"`
	Mode           string  `json:"mode"`
	AllowedTools   string  `json:"allowed_tools"`
	CredentialID   int64   `json:"credential_id"`
	Preset         string  `json:"preset"`  // empty means signed in from the terminal, on a subscription
	Account        string  `json:"account"` // who a subscription agent was last seen signed in as; never a token
	AcceptsQueue   bool    `json:"accepts_queue"`
	Token          string  `json:"-"`
	MaxRunUSD      float64 `json:"max_run_usd"`
	DailyBudgetUSD float64 `json:"daily_budget_usd"`
	RunLimitSec    int     `json:"run_limit_sec"`
	PauseReason    string  `json:"pause_reason"`
	Session        string  `json:"session"`
	CreatedAt      int64   `json:"created_at"`
	Running        bool    `json:"running"`
}

// Subscription says the agent bills to a plan rather than per token.
func (ag Agent) Subscription() bool { return ag.CredentialID == 0 }

const agentCols = `a.id,a.name,a.cli,a.model,a.mode,a.allowed_tools,COALESCE(a.credential_id,0),COALESCE(c.preset,''),
	a.accepts_queue,a.token,a.max_run_usd,a.daily_budget_usd,a.run_limit_sec,a.pause_reason,a.session,a.created_at,a.account`

func scanAgent(s interface{ Scan(...any) error }) (Agent, error) {
	var ag Agent
	err := s.Scan(&ag.ID, &ag.Name, &ag.CLI, &ag.Model, &ag.Mode, &ag.AllowedTools, &ag.CredentialID, &ag.Preset,
		&ag.AcceptsQueue, &ag.Token, &ag.MaxRunUSD, &ag.DailyBudgetUSD, &ag.RunLimitSec, &ag.PauseReason, &ag.Session, &ag.CreatedAt, &ag.Account)
	return ag, err
}

func (a *App) agent(id int64) (Agent, error) {
	return scanAgent(a.db.QueryRow(`SELECT `+agentCols+` FROM agents a LEFT JOIN credentials c ON c.id=a.credential_id WHERE a.id=?`, id))
}

func (a *App) agentByToken(token string) (Agent, error) {
	return scanAgent(a.db.QueryRow(`SELECT `+agentCols+` FROM agents a LEFT JOIN credentials c ON c.id=a.credential_id WHERE a.token=?`, token))
}

func (a *App) agents() ([]Agent, error) {
	rows, err := a.db.Query(`SELECT ` + agentCols + ` FROM agents a LEFT JOIN credentials c ON c.id=a.credential_id ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		ag, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ag)
	}
	return out, rows.Err()
}

// validateAgent checks an agent before it's created. Trust boundary: the model
// and tool lists end up as CLI arguments.
func (a *App) validateAgent(ag *Agent) error {
	if strings.TrimSpace(ag.Name) == "" {
		return errors.New("a name is required")
	}
	if ag.CredentialID != 0 {
		var preset string
		if err := a.db.QueryRow(`SELECT preset FROM credentials WHERE id=?`, ag.CredentialID).Scan(&preset); err != nil {
			return errors.New("that credential doesn't exist")
		}
		if p, _ := presetByKey(preset); p.CLI != ag.CLI {
			return fmt.Errorf("a %s credential can't be used with %s", preset, ag.CLI)
		}
		ag.Preset = preset
	} else if ag.CLI != "claude" {
		return fmt.Errorf("%s agents need a credential: subscription sign-in is only offered for Claude Code", ag.CLI)
	}
	if err := validMode(ag.CLI, ag.Preset, ag.Mode); err != nil {
		return err
	}
	if strings.HasPrefix(ag.Model, "-") || strings.ContainsAny(ag.Model, " \n\t") {
		return errors.New("that model name isn't valid")
	}
	// Codex and agy don't reliably name their model, and pricing needs one.
	if ag.Model == "" && (ag.CLI != "claude" || ag.Preset == "gateway" || ag.Preset == "gateway_other") {
		return errors.New("this agent needs a model name so its usage can be priced")
	}
	if ag.Mode == ModeAllowlist && strings.TrimSpace(ag.AllowedTools) == "" {
		return errors.New("allowlist mode needs a list of allowed tools")
	}
	if ag.RunLimitSec <= 0 {
		ag.RunLimitSec = 3600
	}
	if ag.MaxRunUSD < 0 || ag.DailyBudgetUSD < 0 {
		return errors.New("budgets can't be negative")
	}
	return nil
}

func (a *App) createAgent(ag *Agent) error {
	if err := a.validateAgent(ag); err != nil {
		return err
	}
	ag.Token = randomToken()
	var cred any
	if ag.CredentialID != 0 {
		cred = ag.CredentialID
	}
	res, err := a.db.Exec(`INSERT INTO agents(name,cli,model,mode,allowed_tools,credential_id,accepts_queue,token,max_run_usd,daily_budget_usd,run_limit_sec,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, ag.Name, ag.CLI, ag.Model, ag.Mode, ag.AllowedTools, cred, ag.AcceptsQueue, ag.Token, ag.MaxRunUSD, ag.DailyBudgetUSD, ag.RunLimitSec, now())
	if err != nil {
		return fmt.Errorf("couldn't save agent: %w", err)
	}
	ag.ID, _ = res.LastInsertId()
	return nil
}

// ---- session marks

func (a *App) marks(session string) Usage {
	var raw string
	m := Usage{}
	if a.db.QueryRow(`SELECT marks FROM sessions WHERE id=?`, session).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func (a *App) saveMarks(session string, m Usage) error {
	b, _ := json.Marshal(m)
	_, err := a.db.Exec(`INSERT INTO sessions(id,marks) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET marks=excluded.marks`, session, string(b))
	return err
}

// ---- prices

func (a *App) prices() (map[string]Price, error) {
	rows, err := a.db.Query(`SELECT model,input,output,cache_read,cache_write,checked_at FROM prices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Price{}
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.CheckedAt); err != nil {
			return nil, err
		}
		out[p.Model] = p
	}
	return out, rows.Err()
}

func (a *App) setPrice(p Price) error {
	if p.Model == "" || p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 {
		return errors.New("a model name and non-negative prices are required")
	}
	if p.CheckedAt == "" {
		p.CheckedAt = time.Now().UTC().Format("2006-01-02")
	}
	_, err := a.db.Exec(`INSERT INTO prices(model,input,output,cache_read,cache_write,checked_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(model) DO UPDATE SET input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,checked_at=excluded.checked_at`,
		p.Model, p.Input, p.Output, p.CacheRead, p.CacheWrite, p.CheckedAt)
	return err
}

// ---- events

// addEvent stores a feed entry and publishes it. Deltas are published only.
func (a *App) addEvent(agentID, runID int64, e Event) {
	b, _ := json.Marshal(e)
	var id int64
	if e.Kind != "delta" {
		res, err := a.db.Exec(`INSERT INTO events(agent_id,run_id,ts,data) VALUES(?,?,?,?)`, agentID, runID, now(), string(b))
		if err == nil {
			id, _ = res.LastInsertId()
		}
	}
	a.hub.publish(Message{ID: id, AgentID: agentID, RunID: runID, TS: now(), Data: b})
}

func (a *App) eventsAfter(agentID, after int64, limit int) ([]Message, error) {
	q := `SELECT id,agent_id,run_id,ts,data FROM (SELECT * FROM events WHERE agent_id=? AND id>? ORDER BY id DESC LIMIT ?) ORDER BY id`
	rows, err := a.db.Query(q, agentID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var data string
		if err := rows.Scan(&m.ID, &m.AgentID, &m.RunID, &m.TS, &data); err != nil {
			return nil, err
		}
		m.Data = json.RawMessage(data)
		out = append(out, m)
	}
	return out, rows.Err()
}

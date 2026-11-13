package config

// DefaultDesktopApp is claude.desktopApp's default, the desktop app's
// executable on Linux.
const DefaultDesktopApp = "claude-desktop"

// Claude locates what Claude Code and the desktop app keep on disk.
type Claude struct {
	ProjectsDir string `yaml:"projectsDir"`
	// DesktopApp is the desktop app's executable, which starts the app or
	// hands a claude:// link to the running one.
	DesktopApp string `yaml:"desktopApp"`
	// SessionsDir holds the record every running CLI keeps of the session
	// it runs (<pid>.json), what `claude agents` lists.
	SessionsDir string `yaml:"sessionsDir"`
	DesktopDir  string `yaml:"desktopDir"`
	// DesktopLog is the desktop app's main log: its latest focus change
	// says which session the main window shows.
	DesktopLog string `yaml:"desktopLog"`
}

// Omp locates what omp (oh-my-pi), the second local harness, keeps on disk.
type Omp struct {
	// SessionsDir holds omp's session files, one folder per working
	// directory.
	SessionsDir string `yaml:"sessionsDir"`
	// ModelsFile is omp's provider configuration (~/.omp/agent/models.yml):
	// the providers' endpoints and models, and for a provider of Providers
	// the name of the variable omp reads its key from.
	ModelsFile string `yaml:"modelsFile"`
	// Model is the model `agents start --harness omp` starts an agent on
	// without --model, an exact selector omp lists ("ollama/qwen3.5:9b");
	// empty: such a start is refused.
	Model string `yaml:"model"`
	// ServedBy is the lease resource serving Model. Starts refuse while held.
	ServedBy string `yaml:"servedBy"`
	// Providers are the providers of ModelsFile whose key lives in the
	// vault, by the provider's name there.
	Providers map[string]OmpProvider `yaml:"providers"`
}

// OmpProvider is a provider of omp's models file whose key beekeeper hands
// to the agents it starts on it.
type OmpProvider struct {
	// APIKey is the op:// reference of the provider's key, never the key:
	// `agents start --harness omp` reads it through beekeeper's secret
	// handling and puts it into the agent's environment under the variable
	// the provider's apiKey in ModelsFile names, so no file carries the
	// value.
	APIKey string `yaml:"apiKey"`
}

// Agy locates what agy (Antigravity CLI), the third local harness, keeps on
// disk and how `agents start --harness agy` launches it.
type Agy struct {
	// BrainDir holds agy's conversation folders, each with
	// .system_generated/logs/transcript.jsonl (default
	// ~/.gemini/antigravity-cli/brain).
	BrainDir string `yaml:"brainDir"`
	// ProjectsDir holds agy's project definitions (~/.gemini/config/projects),
	// used to match a working directory to `--project <id>` on start.
	ProjectsDir string `yaml:"projectsDir"`
	// ConversationsDir holds agy's conversation databases
	// (~/.gemini/antigravity-cli/conversations).
	ConversationsDir string `yaml:"conversationsDir"`
	// RemoteControl passes --remote-control to `agy` when starting an agent.
	RemoteControl bool `yaml:"remoteControl"`
	// RemoteDaemon is the systemd user service of agy's remote view daemon
	// (e.g. antigravity-cli-daemon): when set, `agents start --harness agy`
	// runs `systemctl --user try-restart <remoteDaemon>` once the new
	// conversation's database exists so the remote view lists it.
	RemoteDaemon string `yaml:"remoteDaemon"`
}

// Codex locates what Codex CLI keeps on disk and configures codex worker starts.
type Codex struct {
	// SessionsDir holds Codex rollout files (~/.codex/sessions).
	SessionsDir string `yaml:"sessionsDir"`
	// AuthFile is the Codex OAuth file symlinked into an isolated worker
	// CODEX_HOME (~/.codex/auth.json).
	AuthFile string `yaml:"authFile"`
	// ConfigFile is the user's Codex config.toml whose [projects.*] trust
	// tables are copied into an isolated worker CODEX_HOME (~/.codex/config.toml).
	ConfigFile string `yaml:"configFile"`
	// Model is the model `agents start --harness codex` starts an agent on
	// without --model.
	Model string `yaml:"model"`
}

// Desktop is how beekeeper shares the person's desktop.
type Desktop struct {
	// TypingQuiet is how long the person's keyboards and pointers stay
	// idle before a claude:// link switches the desktop's window (the
	// import of agents start, a reopen): a stray keystroke goes into the
	// window the link opens. A negative value opens links without
	// watching the input. A browse turn draws the windows Chrome opens
	// off the person's screen only while their input stays quiet this long,
	// and watches none when it is negative.
	TypingQuiet Duration `yaml:"typingQuiet"`
}

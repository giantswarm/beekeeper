package guard

import "testing"

func TestRecoveryCommands(t *testing.T) {
	for _, command := range []string{"pwd", "ls -l", codexGitStatus, "beekeeper config set watch.interval 1m", "beekeeper self-update", "cd /tmp && beekeeper config check"} {
		if !recoveryCommand(command) {
			t.Errorf("refused recovery: %s", command)
		}
	}
	for _, command := range []string{"rm config.yaml", "ls; rm config.yaml", "ls > config.yaml", "ls $(touch marker)", "beekeeper config set x `touch marker`", "sh -c ls", "find . -exec touch marker ;", "X=x beekeeper version", "git diff", "function ls() { rm config.yaml; }; ls", "ls <(touch marker)", "ls ${X:=value}"} {
		if recoveryCommand(command) {
			t.Errorf("allowed unsafe command: %s", command)
		}
	}
}

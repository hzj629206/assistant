package agent

import "testing"

func TestSlashCommandValidateStopMute(t *testing.T) {
	t.Parallel()

	command, ok := ParseSlashCommand(" /stop mute ")
	if !ok {
		t.Fatal("expected slash command to be parsed")
	}
	if !command.Validate() {
		t.Fatal("expected /stop mute to be valid")
	}
	if !command.IsStop() || command.Args != slashCommandStopMute {
		t.Fatalf("expected stop mute command, got %+v", command)
	}
}

func TestSlashCommandRejectsUnsupportedStopArgument(t *testing.T) {
	t.Parallel()

	command, ok := ParseSlashCommand("/stop later")
	if !ok {
		t.Fatal("expected slash command to be parsed")
	}
	if command.Validate() {
		t.Fatal("expected unsupported stop argument to be invalid")
	}
}

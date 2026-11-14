package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Runner runs an external command and returns its standard output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands with os/exec. A failing command's standard
// error is included in the returned error.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

const (
	holdSinkName  = "rbt_hold"
	configuredKey = "default.configured.audio.sink"
	metadataType  = "Spa:String:JSON"
)

type card struct {
	Name          string `json:"name"`
	ActiveProfile string `json:"active_profile"`
}

type sink struct {
	Index       int             `json:"index"`
	Name        string          `json:"name"`
	OwnerModule json.RawMessage `json:"owner_module"`
}

// owner returns the id of the module that created the sink, "" if unknown.
func (s sink) owner() string {
	return strings.Trim(string(s.OwnerModule), `"`)
}

type sinkInput struct {
	Index int `json:"index"`
	Sink  int `json:"sink"`
}

// pactl wraps the pactl and pw-metadata commands the Manager needs.
type pactl struct{ r Runner }

func (p pactl) listJSON(ctx context.Context, what string, v any) error {
	out, err := p.r.Run(ctx, "pactl", "-f", "json", "list", what)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("audio: parse pactl list %s: %w", what, err)
	}
	return nil
}

func (p pactl) cards(ctx context.Context) ([]card, error) {
	var c []card
	return c, p.listJSON(ctx, "cards", &c)
}

func (p pactl) sinks(ctx context.Context) ([]sink, error) {
	var s []sink
	return s, p.listJSON(ctx, "sinks", &s)
}

func (p pactl) sinkInputs(ctx context.Context) ([]sinkInput, error) {
	var s []sinkInput
	return s, p.listJSON(ctx, "sink-inputs", &s)
}

func (p pactl) defaultSink(ctx context.Context) (string, error) {
	out, err := p.r.Run(ctx, "pactl", "get-default-sink")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// configuredSink reads default.configured.audio.sink. set is false when the
// key has no value.
func (p pactl) configuredSink(ctx context.Context) (value string, set bool, err error) {
	out, err := p.r.Run(ctx, "pw-metadata", "0", configuredKey)
	if err != nil {
		return "", false, err
	}
	value, set = parseConfiguredSink(out)
	return value, set, nil
}

var updateValueRe = regexp.MustCompile(`value:'(.*)' type:'`)

// parseConfiguredSink finds the value in pw-metadata's
// "update: id:0 key:'default.configured.audio.sink' value:'…' type:'…'" line.
func parseConfiguredSink(out []byte) (string, bool) {
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "update:") || !strings.Contains(line, "key:'"+configuredKey+"'") {
			continue
		}
		if m := updateValueRe.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
	}
	return "", false
}

func (p pactl) setConfiguredSink(ctx context.Context, value string) error {
	_, err := p.r.Run(ctx, "pw-metadata", "0", configuredKey, value, metadataType)
	return err
}

func (p pactl) deleteConfiguredSink(ctx context.Context) error {
	_, err := p.r.Run(ctx, "pw-metadata", "-d", "0", configuredKey)
	return err
}

// loadHoldSink loads the null sink streams are parked on and returns its
// module id.
func (p pactl) loadHoldSink(ctx context.Context) (string, error) {
	out, err := p.r.Run(ctx, "pactl", "load-module", "module-null-sink",
		"sink_name="+holdSinkName, "sink_properties=device.description=Released-for-handoff")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("audio: unexpected load-module output %q", id)
	}
	return id, nil
}

func (p pactl) unloadModule(ctx context.Context, id string) error {
	_, err := p.r.Run(ctx, "pactl", "unload-module", id)
	return err
}

func (p pactl) setDefaultSink(ctx context.Context, name string) error {
	_, err := p.r.Run(ctx, "pactl", "set-default-sink", name)
	return err
}

func (p pactl) moveSinkInput(ctx context.Context, index int, sinkName string) error {
	_, err := p.r.Run(ctx, "pactl", "move-sink-input", strconv.Itoa(index), sinkName)
	return err
}

func (p pactl) setCardProfile(ctx context.Context, card, profile string) error {
	_, err := p.r.Run(ctx, "pactl", "set-card-profile", card, profile)
	return err
}

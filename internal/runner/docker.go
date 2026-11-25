package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Docker runs backends as local containers, with their ports on localhost:
// for development and the end-to-end test.
type Docker struct {
	Prefix string
	// Network, if set, is a docker network to join too.
	Network string
	// Logs, if set, gets a backend's last lines when it's stopped.
	Logs io.Writer
}

func (d *Docker) docker(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // docker is what this runner runs

	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (d *Docker) Start(ctx context.Context, gen int, dir string, spec Spec) (Backend, error) {
	name := Name(d.Prefix, gen)
	args := []string{
		"run", "-d", "--name", name, "--label", "molt.gen=" + strconv.Itoa(gen),
		"-v", dir + ":/data",
		"-p", fmt.Sprintf("127.0.0.1::%d", GamePort), "-p", fmt.Sprintf("127.0.0.1::%d", AgentPort),
	}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	// The server runs as whoever owns its directory: us.
	if _, ok := spec.Env["UID"]; !ok {
		args = append(args, "-e", "UID="+strconv.Itoa(os.Getuid()), "-e", "GID="+strconv.Itoa(os.Getgid()))
	}
	for _, e := range env(spec) {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image)
	if _, err := d.docker(ctx, args...); err != nil {
		return Backend{}, err
	}
	b, err := d.backend(ctx, gen)
	if err != nil {
		_, _ = d.docker(context.WithoutCancel(ctx), "rm", "-f", name)
	}
	return b, err
}

func (d *Docker) backend(ctx context.Context, gen int) (Backend, error) {
	name := Name(d.Prefix, gen)
	port := func(p int) (string, error) {
		out, err := d.docker(ctx, "port", name, fmt.Sprintf("%d/tcp", p))
		if err != nil {
			return "", err
		}
		// One line per address family; the first is localhost.
		return strings.Fields(out)[0], nil
	}
	game, err := port(GamePort)
	if err != nil {
		return Backend{}, err
	}
	agent, err := port(AgentPort)
	if err != nil {
		return Backend{}, err
	}
	return Backend{Gen: gen, Name: name, Game: game, Agent: "http://" + agent}, nil
}

func (d *Docker) Stop(ctx context.Context, b Backend) error {
	if d.Logs != nil {
		out, _ := d.docker(ctx, "logs", "--tail", "150", b.Name)
		fmt.Fprintf(d.Logs, "=== %s\n%s\n", b.Name, out)
	}
	if _, err := d.docker(ctx, "stop", "-t", "60", b.Name); err != nil {
		return err
	}
	_, err := d.docker(ctx, "rm", b.Name)
	return err
}

func (d *Docker) Find(ctx context.Context, gen int) (Backend, bool, error) {
	out, err := d.docker(ctx, "ps", "-q", "--filter", "name=^"+Name(d.Prefix, gen)+"$")
	if err != nil || out == "" {
		return Backend{}, false, err
	}
	b, err := d.backend(ctx, gen)
	return b, err == nil, err
}

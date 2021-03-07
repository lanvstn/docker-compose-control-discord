package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/sync/errgroup"
)

type monitoredContainers struct {
	active map[string]struct{}
	sync.RWMutex
}

type appState struct {
	monitoredContainers monitoredContainers

	state map[string]interface{}
	sync.RWMutex

	buf      *bytes.Buffer
	handlers []logToStateFunc

	dockerClient *client.Client
}

type logToStateFunc func(string, map[string]interface{}) string

func newAppState(dcli *client.Client, handlers []logToStateFunc) *appState {
	return &appState{
		dockerClient: dcli,
		state:        make(map[string]interface{}),
		buf:          &bytes.Buffer{},
		handlers:     handlers,
		monitoredContainers: monitoredContainers{
			active: make(map[string]struct{}),
		},
	}
}

func (as *appState) clearState() {
	as.Lock()
	as.state = make(map[string]interface{})
	as.Unlock()
}

func (as *appState) readFromContainer(ctx context.Context, container, since string) error {
	c, err := as.dockerClient.ContainerInspect(ctx, container)
	if err != nil {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	opts := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      since,
		Timestamps: true,
		Follow:     true,
	}

	raw, err := as.dockerClient.ContainerLogs(ctx, container, opts)
	if err != nil {
		return fmt.Errorf("failed to start following logs: %w", err)
	}
	log.Printf("container logstream connected for %v\n", container)

	g := new(errgroup.Group)

	if !c.Config.Tty {
		stderrW := &bytes.Buffer{}
		stdoutW := &bytes.Buffer{}

		// io.Copy will return when the buffers above are empty.
		// This is a trick to make copy restart as long as the demuxer (which only exits on actual EOF)
		// is still running. There are probably better ways to do this...
		muxClosed := false

		g.Go(func() error {
			_, err := stdcopy.StdCopy(stderrW, stdoutW, raw)
			if err != nil {
				return fmt.Errorf("error demultiplexing docker logstream: %w", err)
			}
			log.Println("logstream closed")
			muxClosed = true
			return nil
		})
		g.Go(func() error {
			for {
				if muxClosed {
					return nil
				}

				_, err := io.Copy(as.buf, stdoutW)
				if err != nil {
					return fmt.Errorf("error copying stdout: %w", err)
				}
				time.Sleep(500 * time.Millisecond)
			}

		})
		g.Go(func() error {
			for {
				if muxClosed {
					return nil
				}

				_, err := io.Copy(as.buf, stderrW)
				if err != nil {
					return fmt.Errorf("error copying stderr: %w", err)
				}
				time.Sleep(500 * time.Millisecond)
			}
		})
	} else {
		g.Go(func() error {
			_, err := io.Copy(as.buf, raw)
			if err != nil {
				return fmt.Errorf("error copying logs: %w", err)
			}
			log.Println("simple copy exit")
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	return nil
}

func (as *appState) handleMessages(ctx context.Context, events chan<- string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			msg, err := as.buf.ReadString('\n')
			if err == io.EOF {
				time.Sleep(1 * time.Second) // wait 1s till checking for messages again. is there a better way?
				continue
			}
			if err != nil {
				events <- fmt.Sprintf("error reading from buffer: %v", err)
				return err
			}

			for _, h := range as.handlers {
				as.Lock()
				out := h(msg, as.state)
				as.Unlock()
				if out != "" {
					events <- out
				}
			}
		}
	}
}

func (as *appState) monitorContainer(ctx context.Context, container string) error {
	as.monitoredContainers.Lock()
	defer as.monitoredContainers.Unlock()

	if _, ok := as.monitoredContainers.active[container]; ok {
		log.Printf("already monitoring container %v\n", container)
		return nil
	}

	_, err := as.dockerClient.ContainerInspect(ctx, container)
	if err != nil {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	go func() {
		err := as.readFromContainer(ctx, container, lastExit.Format(time.RFC3339))
		if err != nil {
			log.Printf("error reading from container: %v\n", err)
		}
		log.Printf("stopped reading from %v\n", container)
	}()

	return nil
}

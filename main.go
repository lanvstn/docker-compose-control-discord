package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/client"
)

const (
	prefix                = "%"
	requiredRole          = "valheim"
	adminRole             = "admin"
	lockDurationStartStop = 10 * time.Second
	lockDurationStatus    = 5 * time.Second
)

var (
	testMode          bool
	targetProjectName string
	targetService     string
)

// TODO: move these vars to somewhere not global

var (
	cancelStopCh        chan struct{}
	lock                uint32
	lastExit            time.Time
	as                  *appState
	dcli                *client.Client
	guildCreateReceived chan struct{}
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	flag.BoolVar(&testMode, "test", false, "enable test mode")
	flag.StringVar(&targetProjectName, "target-project-name", "", "docker compose project name of the target project, default: name of cwd")
	flag.StringVar(&targetService, "target-service", "", "service in the docker compose project to use for the appState")
	flag.Parse()

	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("no token")
	}

	if targetProjectName == "" {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		targetProjectName = path.Base(cwd)
	}

	var err error
	lastExit, err = readExitTime()
	if err != nil {
		log.Fatalf("failed to read exit time: %v", err)
	}

	dcli, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatalf("failed to init dockerclient: %v", err)
	}

	as = newAppState(dcli, valheimStateHandlerFuncs)
	events := make(chan string)
	go as.handleMessages(ctx, events)

	guildCreateReceived = make(chan struct{}) // TODO proper guild state tracking?

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("failed creating bot: %s", err.Error())
	}

	dg.AddHandler(messageCreate)
	dg.AddHandler(guildCreate)

	dg.State.TrackRoles = true
	dg.StateEnabled = true
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuilds

	err = dg.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}
	defer dg.Close()

	go func() {
		<-guildCreateReceived
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-events:
				broadcast(dg, msg)
			}
		}
	}()

	fmt.Println("docker-compose-control bot is now running.")

	_ = monitorTarget() // try to resume monitoring

	defer func() {
		err := writeExitTime()
		if err != nil {
			log.Printf("failed to write exit time: %v", err)
		}
	}()

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc
}

func broadcast(s *discordgo.Session, msg string) {
	log.Printf("broadcast: %v\n", msg)
	for _, g := range s.State.Guilds {
		for _, c := range g.Channels {
			if c.Name == "general" { // TODO support choosing broadcast channel
				_, err := s.ChannelMessageSend(c.ID, msg)
				if err != nil {
					log.Printf("broadcast failed to guild %v: %v\n", g.ID, err)
				}
				continue
			}
		}
	}
}

func guildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	log.Printf("guild create: %v (%v)", g.Guild.ID, g.Guild.Name)
	guildCreateReceived <- struct{}{}
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	if !strings.HasPrefix(m.Content, prefix) {
		return
	}

	defer func() {
		if err := recover(); err != nil {
			log.Printf("PANIC while processing message: %s", err)
			debug.PrintStack()
		}
	}()

	if !userAllowed(s, m, requiredRole) {
		log.Printf("user %v does not have required role %s in %v", m.Author.String(), requiredRole, m.Member.Roles)
		respond(s, m, "Permission denied")
		return
	}

	fullCmd := strings.TrimPrefix(m.Content, prefix)
	log.Printf("processing command: %s\n", fullCmd)

	cmd := strings.Split(fullCmd, " ")
	if len(cmd) == 0 {
		return
	}

	switch cmd[0] {
	case "start":
		if !lockDuration(lockDurationStartStop) {
			respond(s, m, "Calm the fuck down")
			return
		}

		if err := startServer(); err != nil {
			respond(s, m, "Failed to start server: %s", err.Error())
			return
		}

		if err := monitorTarget(); err != nil {
			respond(s, m, fmt.Sprintf("Server has been started but: %v", err))
			return
		}

		respond(s, m, "Server has been started")
		return
	case "stop":
		var delay time.Duration

		if len(cmd) == 2 {
			if !userAllowed(s, m, adminRole) {
				respond(s, m, "Permission denied, use stop command without parameters")
				return
			}
			if cmd[1] == "now" {
				delay = 10 * time.Second
			} else {
				respond(s, m, "Invalid parameter, options: now")
				return
			}
		} else {
			delay = 10 * time.Minute
		}

		if !lockDuration(lockDurationStartStop) {
			respond(s, m, "Calm the fuck down")
			return
		}

		if err := delayedStopServer(delay, func(msg string) { respond(s, m, msg) }); err != nil {
			respond(s, m, "Failed to intiate server stop: %s", err.Error())
			return
		}
		respond(s, m, "Stopping server in %s", delay.String())
		return
	case "cancel":
		if err := cancelStop(); err != nil {
			respond(s, m, "Failed to cancel stop: %s", err.Error())
			return
		}
		respond(s, m, "Canceled stop")
		return
	case "status":
		if !lockDuration(lockDurationStatus) {
			respond(s, m, "Calm the fuck down")
			return
		}

		if getStatus() {
			respond(s, m, "Server is currently up.")
		} else {
			respond(s, m, "Server is currently down.")
		}
		return
	default:
		respond(s, m, "Unknown command")
		return
	}
}

func respond(s *discordgo.Session, m *discordgo.MessageCreate, msg string, args ...interface{}) {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}

	_, err := s.ChannelMessageSend(m.ChannelID, msg)
	if err != nil {
		log.Printf("message send error: %s\n", err)
	}
}

func userAllowed(s *discordgo.Session, m *discordgo.MessageCreate, wantedRole string) bool {
	for _, roleID := range m.Member.Roles {
		role, err := s.State.Role(m.GuildID, roleID)

		if err != nil {

			if errors.Is(err, discordgo.ErrStateNotFound) {

				if err := updateRoleState(s, m.GuildID); err != nil {
					log.Printf("failed updating role state: %s", err.Error())
					return false
				}

				role, err = s.State.Role(m.GuildID, roleID)
				if err != nil {
					log.Printf("failed to get role after updating: %s", err.Error())
					return false
				}

			} else {
				log.Printf("failed reading role from state: %s", err.Error())
				return false
			}
		}

		if role.Name == wantedRole {
			return true
		}
	}

	return false
}

func updateRoleState(s *discordgo.Session, guildID string) error {
	roles, err := s.GuildRoles(guildID)
	if err != nil {
		return err
	}

	for _, role := range roles {
		if err := s.State.RoleAdd(guildID, role); err != nil {
			return err
		}
	}
	// TODO drop old roles from state
	return nil
}

func delayedStopServer(delay time.Duration, onStopped func(msg string)) error {
	if cancelStopCh != nil {
		return errors.New("a server stop is already pending")
	}

	// TODO race conditions
	cancelStopCh = make(chan struct{})

	go func() {
		select {
		case <-time.After(delay):
			_ = lockDuration(lockDurationStartStop)
			err := stopServer()
			if err != nil {
				onStopped(fmt.Sprintf("error stopping server: %s", err.Error()))
			} else {
				onStopped("server stopped")
			}
		case <-cancelStopCh:
		}
		cancelStopCh = nil
	}()

	return nil
}

func cancelStop() error {
	if cancelStopCh != nil {
		cancelStopCh <- struct{}{}
	}
	return nil
}

// lockDuration locks in the background for duration if possible
//  and returns true if able to lock
func lockDuration(d time.Duration) bool {
	if atomic.CompareAndSwapUint32(&lock, 0, 1) {
		go func() {
			<-time.After(d)
			atomic.StoreUint32(&lock, 0)
		}()
		return true
	}
	return false
}

func runCommand(name string, args ...string) error {
	if testMode {
		log.Printf("test mode: run command: %v %v", name, args)
		return nil
	}

	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("command error: %s, out: %q", err.Error(), out)
		return errors.New("error executing command, check logs")
	}

	return nil
}

func monitorTarget() error {
	c, err := appContainer(context.TODO(), dcli, targetProjectName, targetService)
	if err != nil {
		return fmt.Errorf("could not find the container so monitoring will fail: %w", err)
	}

	log.Printf("found appContainer %v\n", c.ID)

	if err := as.monitorContainer(context.Background(), c.ID); err != nil {
		return fmt.Errorf("could not start monitoring the container after finding it: %w", err)
	}

	return nil
}

func writeExitTime() error {
	return os.WriteFile("stoptime", []byte(strconv.Itoa(int(time.Now().Unix()))), 0o600)
}

func readExitTime() (time.Time, error) {
	content, err := os.ReadFile("stoptime")
	if os.IsNotExist(err) {
		return time.Time{}, nil
	}

	i, err := strconv.Atoi(string(content))
	if err != nil {
		return time.Time{}, err
	}

	return time.Unix(int64(i), 0), nil
}

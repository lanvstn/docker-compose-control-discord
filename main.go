package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	prefix                = "%"
	requiredRole          = "valheim"
	adminRole             = "admin"
	lockDurationStartStop = 10 * time.Second
	lockDurationStatus    = 5 * time.Second
)

var testMode bool

var (
	cancelStopCh chan struct{}
	lock         uint32
)

func main() {
	flag.BoolVar(&testMode, "test", false, "enable test mode")
	flag.Parse()

	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("no token")
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("failed creating bot: %s", err.Error())
	}

	dg.AddHandler(messageCreate)

	dg.State.TrackRoles = true
	dg.StateEnabled = true

	dg.Identify.Intents = discordgo.IntentsGuildMessages

	err = dg.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}
	defer dg.Close()

	// Wait here until CTRL-C or other term signal is received.
	fmt.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc
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
		}
	}()

	if !userAllowed(s, m, requiredRole) {
		log.Printf("user %v does not have required role %s in %v", m.Author.String(), requiredRole, m.Member.Roles)
		respond(s, m, "permission denied")
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
			respond(s, m, "calm the fuck down")
			return
		}

		if err := startServer(); err != nil {
			respond(s, m, "failed to start server: %s", err.Error())
			return
		}
		respond(s, m, "server has been started")
		return
	case "stop":
		var delay time.Duration

		if len(cmd) == 2 {
			if !userAllowed(s, m, adminRole) {
				respond(s, m, "permission denied, use stop command without parameters")
				return
			}
			if cmd[1] == "now" {
				delay = 10 * time.Second
			} else {
				respond(s, m, "invalid parameter, options: now")
				return
			}
		} else {
			delay = 10 * time.Minute
		}

		if !lockDuration(lockDurationStartStop) {
			respond(s, m, "calm the fuck down")
			return
		}

		if err := delayedStopServer(delay, func(msg string) { respond(s, m, msg) }); err != nil {
			respond(s, m, "failed to intiate server stop: %s", err.Error())
			return
		}
		respond(s, m, "stopping server in %s", delay.String())
		return
	case "cancel":
		if err := cancelStop(); err != nil {
			respond(s, m, "failed to cancel stop: %s", err.Error())
			return
		}
		respond(s, m, "canceled stop")
		return
	case "status":
		if !lockDuration(lockDurationStatus) {
			respond(s, m, "calm the fuck down")
			return
		}

		respond(s, m, getStatus())
		return
	default:
		respond(s, m, "unknown command")
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

func startServer() error {
	return runCommand("docker-compose", "up", "-d")
}

func stopServer() error {
	return runCommand("docker-compose", "down")
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

func getStatus() string {
	if testMode {
		return "test mode, status unknown"
	}

	out, err := exec.Command("docker-compose", "top").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("failed to get status: %s", err.Error())
	}

	if strings.TrimSpace(string(out)) == "" {
		return "server currently down"
	}

	return "server currently up"
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

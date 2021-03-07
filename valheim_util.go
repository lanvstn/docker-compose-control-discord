package main

import (
	"fmt"
	"log"
	"regexp"
	"strings"
)

var valheimStateHandlerFuncs = []logToStateFunc{gameServerConnected, playerJoin, steamIDConnect, steamIDDisonnect}

var (
	steamIDConnectRE    = regexp.MustCompile("Got handshake from client ([0-9]+)")
	playerJoinRE        = regexp.MustCompile("Got character ZDOID from ([A-Za-z0-9_-]+)")
	steamIDDisconnectRE = regexp.MustCompile("Closing socket ([0-9]+)")
)

func gameServerConnected(msg string, state map[string]interface{}) string {
	if strings.Contains(msg, "Game server connected") {
		state["connected"] = true
		log.Println("Game server connected")
	}

	return ""
}

func playerJoin(msg string, state map[string]interface{}) string {
	if m := playerJoinRE.FindStringSubmatch(msg); len(m) == 2 {
		// Match playerName with ID
		if cid, ok := state["connectingSteamID"]; ok {
			state[fmt.Sprintf("steamID.%s", cid)] = m[1]
			delete(state, "connectingSteamID")
		}

		return fmt.Sprintf("%s entered the world.", m[1])
	}

	return ""
}

func steamIDConnect(msg string, state map[string]interface{}) string {
	if m := steamIDConnectRE.FindStringSubmatch(msg); len(m) == 2 {
		state[fmt.Sprintf("steamID.%s", m[1])] = "<unknown>"
		state["connectingSteamID"] = m[1]
	}

	return ""
}

func steamIDDisonnect(msg string, state map[string]interface{}) string {
	if m := steamIDDisconnectRE.FindStringSubmatch(msg); len(m) == 2 {
		name, ok := state[fmt.Sprintf("steamID.%s", m[1])]
		if !ok {
			name = "<unknown>"
		}
		return fmt.Sprintf("%s left the world.", name)
	}

	return ""
}

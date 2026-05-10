package gspro

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/brentyates/squaregolf-connector/internal/core"
	"github.com/brentyates/squaregolf-connector/internal/core/simulator"
)

var (
	gsproInstance *Integration
	gsproOnce     sync.Once
)

type Integration struct {
	*simulator.Base
	stateManager   *core.StateManager
	launchMonitor  *core.LaunchMonitor
	shotNumber     int
	lastShotNumber int
	shotListeners  []func(ShotData)
	lastPlayerInfo *PlayerInfo
}

func GetInstance(stateManager *core.StateManager, launchMonitor *core.LaunchMonitor, host string, port int) *Integration {
	gsproOnce.Do(func() {
		gsproInstance = &Integration{
			stateManager:  stateManager,
			launchMonitor: launchMonitor,
			shotListeners: make([]func(ShotData), 0),
		}
		gsproInstance.Base = simulator.NewBase(gsproInstance, host, port)
		gsproInstance.registerStateListeners()
	})
	return gsproInstance
}

func (g *Integration) Name() string {
	return "GSPro"
}

func (g *Integration) DefaultPort() int {
	return 921
}

func (g *Integration) GetStateManager() *core.StateManager {
	return g.stateManager
}

func (g *Integration) GetLaunchMonitor() *core.LaunchMonitor {
	return g.launchMonitor
}

func (g *Integration) SetStatus(status simulator.ConnectionStatus) {
	switch status {
	case simulator.StatusDisconnected:
		g.stateManager.SetGSProStatus(core.GSProStatusDisconnected)
	case simulator.StatusConnecting:
		g.stateManager.SetGSProStatus(core.GSProStatusConnecting)
	case simulator.StatusConnected:
		g.stateManager.SetGSProStatus(core.GSProStatusConnected)
	case simulator.StatusError:
		g.stateManager.SetGSProStatus(core.GSProStatusError)
	}
}

func (g *Integration) SetError(err error) {
	g.stateManager.SetGSProError(err)
}

func (g *Integration) OnConnected() {
	// Send initial ready message to announce ourselves to GSPconnect
	initMessage := ShotData{
		DeviceID:   "CustomLaunchMonitor",
		Units:      "Yards",
		APIversion: "1",
		ShotNumber: 0,
		ShotDataOptions: ShotOptions{
			ContainsBallData:          false,
			ContainsClubData:          false,
			LaunchMonitorIsReady:      true,
			LaunchMonitorBallDetected: false,
		},
	}
	if err := g.sendData(initMessage); err != nil {
		log.Printf("Error sending init message to GSPro: %v", err)
	}

	// Activate ball detection immediately on connect.
	// Some GSPro-compatible software (e.g. DrillsGolf) does not send a
	// "GSPro ready" response, so we cannot rely solely on that message.
	if err := g.launchMonitor.ActivateBallDetection(); err != nil {
		log.Printf("Failed to activate ball detection for GSPro: %v", err)
	}
}

func (g *Integration) OnDisconnected() {
}

func (g *Integration) ProcessMessage(rawMessage string) {
	var baseMsg Message
	if err := json.Unmarshal([]byte(rawMessage), &baseMsg); err != nil {
		log.Printf("Invalid JSON from GSPro: %v", err)
		return
	}

	// Handle the connection-lifecycle messages first (these have no
	// Code field).
	switch baseMsg.Message {
	case "GSPro ready":
		g.handleGSProReadyMessage()
		return
	case "GSPro Player Information":
		var playerInfo PlayerInfo
		if err := json.Unmarshal([]byte(rawMessage), &playerInfo); err != nil {
			log.Printf("Error parsing player info: %v", err)
			return
		}
		g.handlePlayerMessage(&playerInfo)
		g.handleGSProReadyMessage()
		return
	}

	// Code 200 is a successful shot acknowledgement. We accept it
	// regardless of the Message text because different
	// GSPro-compatible sims phrase it differently:
	//   * GSPro itself: "Ball Data received" / "Club & Ball Data received"
	//   * DrillsGolf / OpenGolfSim: "Shot received successfully"
	if baseMsg.Code == 200 {
		log.Printf("Received shot confirmation from GSPro (code=%d): %s", baseMsg.Code, baseMsg.Message)
		g.launchMonitor.ReactivateBallDetectionFromSource("gspro-ack")
		return
	}

	// Code 201 is a player-info update used by Muni Golf and similar
	// non-GSPro sims. Treat it the same way we treat the
	// "GSPro Player Information" message above: parse the embedded
	// Player object, update active-club state, and signal ready so
	// the device gets the right club/handedness for the next shot.
	// (Previously this fell through to a generic 2xx "shot ack"
	// branch which incorrectly re-armed and never updated the club —
	// short putts from a sim that announces club selection via 201
	// would arrive at the device while it was still in the previous
	// club's detection mode, with the wrong sensitivity.)
	if baseMsg.Code == 201 {
		var playerInfo PlayerInfo
		if err := json.Unmarshal([]byte(rawMessage), &playerInfo); err != nil {
			log.Printf("Error parsing player info (code=201): %v", err)
			return
		}
		log.Printf("Received player info from sim (code=201): %s", baseMsg.Message)
		g.handlePlayerMessage(&playerInfo)
		g.handleGSProReadyMessage()
		return
	}

	// Backwards-compatibility fallback for sims that send a known
	// shot-confirmation Message but no Code.
	switch baseMsg.Message {
	case "Ball Data received", "Club & Ball Data received", "Shot received successfully":
		log.Printf("Received shot confirmation from GSPro: %s", baseMsg.Message)
		g.launchMonitor.ReactivateBallDetectionFromSource("gspro-ack-legacy")
	default:
		log.Printf("Unknown GSPro message: code=%d msg=%q", baseMsg.Code, baseMsg.Message)
	}
}

func (g *Integration) handleGSProReadyMessage() {
	err := g.launchMonitor.ActivateBallDetection()
	if err != nil {
		log.Printf("Failed to activate ball detection: %v", err)
		return
	}
}

func (g *Integration) handlePlayerMessage(playerInfo *PlayerInfo) {
	g.lastPlayerInfo = playerInfo

	if clubName := playerInfo.Player.Club; clubName != "" {
		clubType := g.mapGSProClubToInternal(clubName)
		if clubType != nil {
			log.Printf("GSPro selected club: %s (mapped to %v)", clubName, clubType)
			g.stateManager.SetClub(clubType)
		} else {
			log.Printf("Unmapped GSPro club: %s", clubName)
		}

		friendlyName := mapGSProClubToFriendlyName(clubName)
		g.stateManager.SetClubName(&friendlyName)
	}

	if handed := playerInfo.Player.Handed; handed != "" {
		var handednessType core.HandednessType
		if handed == "LH" {
			handednessType = core.LeftHanded
			log.Printf("GSPro selected handedness: Left-handed")
		} else {
			handednessType = core.RightHanded
			log.Printf("GSPro selected handedness: Right-handed")
		}
		g.stateManager.SetHandedness(&handednessType)
	}
}

func (g *Integration) sendData(shotData ShotData) error {
	jsonData, err := json.Marshal(shotData)
	if err != nil {
		return err
	}
	return g.Base.SendMessage(jsonData)
}

func (g *Integration) AddShotListener(listener func(ShotData)) {
	g.shotListeners = append(g.shotListeners, listener)
}

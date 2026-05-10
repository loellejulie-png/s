package core

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	launchMonitorInstance *LaunchMonitor
	launchMonitorOnce     sync.Once
)

// GetLaunchMonitorInstance returns the singleton instance of LaunchMonitor
func GetLaunchMonitorInstance(sm *StateManager, btManager *BluetoothManager) *LaunchMonitor {
	launchMonitorOnce.Do(func() {
		launchMonitorInstance = &LaunchMonitor{
			stateManager:    sm,
			sequence:        0,
			bluetoothClient: btManager.GetClient(),
		}
	})
	return launchMonitorInstance
}

// NewLaunchMonitor is deprecated, use GetLaunchMonitorInstance instead
func NewLaunchMonitor(sm *StateManager, btManager *BluetoothManager) *LaunchMonitor {
	return GetLaunchMonitorInstance(sm, btManager)
}

// LaunchMonitor encapsulates the launch monitor functionality
type LaunchMonitor struct {
	stateManager      *StateManager
	sequence          int
	sequenceMutex     sync.Mutex
	heartbeatCancel   context.CancelFunc
	heartbeatCancelMu sync.Mutex
	bluetoothClient   BluetoothClient

	// Re-arm coalescing. After a shot, multiple paths may try to
	// re-arm ball detection in rapid succession (club-metrics
	// callback, GSPro ack callback, fallback timer). Sending two
	// arming sequences within tens of milliseconds of each other has
	// been observed to leave the device in a state where it stops
	// reporting subsequent low-energy shots — particularly very short
	// putts. We coalesce all re-arms within a short window to a
	// single BLE writes pair.
	rearmMu       sync.Mutex
	lastRearmAt   time.Time
}

// rearmCoalesceWindow is the minimum time between two ActivateBallDetection
// calls triggered by post-shot re-arm paths. Calls inside the window are
// dropped silently — the first call already armed the device and a second
// call would only race against it.
const rearmCoalesceWindow = 800 * time.Millisecond

// UpdateBluetoothClient updates the bluetooth client reference
func (lm *LaunchMonitor) UpdateBluetoothClient(client BluetoothClient) {
	lm.bluetoothClient = client
}

// NotificationHandler handles BLE notifications
func (lm *LaunchMonitor) NotificationHandler(uuid string, data []byte) {
	if len(data) == 0 {
		log.Println("Received empty notification data")
		return
	}

	hexData := hex.EncodeToString(data)

	// Handle battery level notification
	if uuid == BatteryLevelCharUUID {
		batteryLevel := int(data[0])
		lm.stateManager.SetBatteryLevel(&batteryLevel)
		return
	}

	// Split hex string into byte pairs
	var bytesList []string
	for i := 0; i < len(hexData); i += 2 {
		if i+2 <= len(hexData) {
			bytesList = append(bytesList, hexData[i:i+2])
		}
	}

	// Process by byte patterns
	if len(bytesList) >= 2 {
		// Handle alignment notifications (format 11 04)
		if bytesList[0] == "11" && bytesList[1] == "04" {
			lm.HandleAlignmentNotification(bytesList)
			return
		}

		// Sensor notifications (format 11 01)
		if bytesList[0] == "11" && bytesList[1] == "01" {
			lm.HandleSensorNotification(bytesList)
			return
		} else if len(bytesList) >= 3 {
			// Shot Ball Metrics (format 11 02)
			if bytesList[0] == "11" && bytesList[1] == "02" {
				lm.HandleShotBallMetrics(bytesList)
				return
			}
			if bytesList[0] == "11" && bytesList[1] == "03" {
				// Heartbeat from the device
				return
			}
			// OS Version response (format 11 10)
			if bytesList[0] == "11" && bytesList[1] == "10" {
				lm.HandleOSVersionNotification(bytesList)
				return
			}
			// Shot Club Metrics (format 11 07 0f)
			if bytesList[0] == "11" && bytesList[1] == "07" && bytesList[2] == "0f" {
				lm.HandleShotClubMetrics(bytesList)
				return
			}
			// Check for specific "no club data available" response
			if bytesList[0] == "11" && bytesList[1] == "07" && bytesList[2] == "00" {
				// Clear club metrics in state manager to indicate no data is available
				lm.stateManager.SetLastClubMetrics(nil)
				// Re-activate ball detection so the device searches for the next ball
				lm.reactivateBallDetection()
				return
			}
		}
	}
}

// HandleSensorNotification handles sensor notifications (format 11 01)
func (lm *LaunchMonitor) HandleSensorNotification(bytesList []string) {
	sensorData, err := ParseSensorData(bytesList)
	if err != nil {
		log.Printf("Error parsing sensor data: %v", err)
		return
	}

	lm.stateManager.SetBallDetected(sensorData.BallDetected)
	lm.stateManager.SetBallReady(sensorData.BallReady)

	ballPosition := &BallPosition{
		X: sensorData.PositionX,
		Y: sensorData.PositionY,
		Z: sensorData.PositionZ,
	}
	lm.stateManager.SetBallPosition(ballPosition)
}

// HandleAlignmentNotification handles alignment/aim notifications (format 11 04)
func (lm *LaunchMonitor) HandleAlignmentNotification(bytesList []string) {
	alignmentData, err := ParseAlignmentData(bytesList)
	if err != nil {
		log.Printf("Error parsing alignment data: %v", err)
		return
	}

	// Update alignment state - IsAligning is controlled by the UI
	lm.stateManager.SetAlignmentAngle(alignmentData.AimAngle)
	lm.stateManager.SetIsAligned(alignmentData.IsAligned)
}

// HandleShotBallMetrics handles shot ball metrics notifications (format 11 02 37)
func (lm *LaunchMonitor) HandleShotBallMetrics(bytesList []string) {
	shotMetrics, err := ParseShotBallMetrics(bytesList)
	if err != nil {
		log.Printf("Failed to parse shot metrics data: %v", err)
		return
	}

	// Update state manager with ball metrics
	lastBallMetrics := lm.stateManager.GetLastBallMetrics()

	// Convert RawData to string for comparison and storage
	rawDataStr := ""
	for i, b := range shotMetrics.RawData {
		if i > 0 {
			rawDataStr += " "
		}
		rawDataStr += b
	}

	// Check if this is a new shot by comparing raw data
	var lastRawData string
	if lastBallMetrics != nil {
		lastRawData = strings.Join(lastBallMetrics.RawData, " ")
	}

	if lastBallMetrics == nil || lastRawData != rawDataStr {
		log.Printf("LaunchMonitor: Ball metrics — type=%s speed=%.2f m/s VLA=%.2f° HLA=%.2f° spin=%d rpm",
			shotMetrics.ShotType,
			shotMetrics.BallSpeedMPS,
			shotMetrics.VerticalAngle,
			shotMetrics.HorizontalAngle,
			shotMetrics.TotalspinRPM)
		lm.stateManager.SetLastBallMetrics(shotMetrics)

		// Automatically request club metrics after receiving shot metrics
		if lm.bluetoothClient != nil && lm.bluetoothClient.IsConnected() {
			seq := lm.getNextSequence()
			clubMetricsCommand := RequestClubMetricsCommand(seq)

			err := lm.SendCommand(clubMetricsCommand)
			if err != nil {
				log.Printf("Failed to request club metrics: %v", err)
			}
		}

		// Fallback re-arm: if a club-metrics response (0x11 0x07 …)
		// never arrives — which we have observed for very short putts
		// where the device emits ball metrics but no follow-up — the
		// reactivate hooked off HandleShotClubMetrics never fires and
		// the device sits idle, silently dropping the next short putt.
		// Schedule a deferred re-arm and let the club-metrics path
		// pre-empt it via the coalescing window if the response does
		// arrive in time.
		go lm.scheduleFallbackRearm(shotMetrics)
	} else {
		log.Printf("LaunchMonitor: Ball metrics deduped (identical raw data) — type=%s speed=%.2f m/s",
			shotMetrics.ShotType, shotMetrics.BallSpeedMPS)
	}
}

// scheduleFallbackRearm waits ~2.5 s for the device to either deliver
// club-metrics (which would have already triggered reactivateBallDetection
// via the existing handler) or stay silent. If neither the club-metrics
// path nor the GSPro-ack path has fired a re-arm by the timeout, we
// trigger one ourselves. The coalescing window in
// ReactivateBallDetectionFromSource keeps this from racing if a faster
// path also fires.
func (lm *LaunchMonitor) scheduleFallbackRearm(shotMetrics *BallMetrics) {
	time.Sleep(2500 * time.Millisecond)
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return
	}
	lm.ReactivateBallDetectionFromSource(fmt.Sprintf("fallback-%s", shotMetrics.ShotType))
}

// HandleShotClubMetrics handles shot club metrics notifications (format 11 07 0f)
func (lm *LaunchMonitor) HandleShotClubMetrics(bytesList []string) {
	clubMetrics, err := ParseShotClubMetrics(bytesList)
	if err != nil {
		log.Printf("Failed to parse club metrics data: %v", err)
		return
	}

	// Update state manager with club metrics
	lm.stateManager.SetLastClubMetrics(clubMetrics)

	// Re-activate ball detection so the device searches for the next ball
	lm.reactivateBallDetection()
}

// SendCommand sends a command to the BLE device
func (lm *LaunchMonitor) SendCommand(commandHex string) error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	commandBytes, err := hex.DecodeString(commandHex)
	if err != nil {
		return fmt.Errorf("invalid hex command: %w", err)
	}

	err = lm.bluetoothClient.WriteCharacteristic(CommandCharUUID, commandBytes)
	if err != nil {
		return fmt.Errorf("error sending command: %w", err)
	}

	return nil
}

// ReadBatteryLevel reads the battery level from the device
func (lm *LaunchMonitor) ReadBatteryLevel() (int, error) {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return 0, fmt.Errorf("not connected to device")
	}

	batteryLevelBytes, err := lm.bluetoothClient.ReadCharacteristic(BatteryLevelCharUUID)
	if err != nil {
		return 0, fmt.Errorf("could not read battery level: %w", err)
	}

	if len(batteryLevelBytes) == 0 {
		return 0, fmt.Errorf("received empty battery level data")
	}

	batteryLevel := int(batteryLevelBytes[0])

	// Update state manager with battery level
	lm.stateManager.SetBatteryLevel(&batteryLevel)

	return batteryLevel, nil
}

// ActivateBallDetection activates ball detection mode by sending the
// full sequence: club command + DetectBall command. Use this for
// initial connection, club changes, or any path where the active club
// might have changed.
//
// For *post-shot* re-arming where the club hasn't changed, prefer
// rearmDetectionOnly() — re-sending the club command on every shot has
// been observed to disrupt the device's per-club detection tuning,
// causing the next low-energy shot (typically a short putt) to be
// silently dropped.
func (lm *LaunchMonitor) ActivateBallDetection() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	// Get current club, handedness, and spin mode from state
	club := lm.stateManager.GetClub()
	handedness := lm.stateManager.GetHandedness()
	spinMode := lm.stateManager.GetSpinMode()

	// Default to right-handed driver if not set
	if club == nil {
		defaultClub := ClubDriver
		club = &defaultClub
	}
	if handedness == nil {
		defaultHandedness := RightHanded
		handedness = &defaultHandedness
	}
	if spinMode == nil {
		defaultSpinMode := Advanced
		spinMode = &defaultSpinMode
	}

	// Send club command — branch to the swing-stick variant when the
	// user has opted into it. Mirrors the official Square app's
	// preference: never / driver-and-woods / always.
	seq := lm.getNextSequence()
	useSwingStick := false
	switch lm.stateManager.GetSwingStickMode() {
	case SwingStickAll:
		useSwingStick = true
	case SwingStickDriverWoods:
		useSwingStick = IsDriverOrWood(*club)
	}
	var clubCommand string
	if useSwingStick {
		clubCommand = SwingStickCommand(seq, *club, *handedness)
	} else {
		clubCommand = ClubCommand(seq, *club, *handedness)
	}

	err := lm.SendCommand(clubCommand)
	if err != nil {
		return fmt.Errorf("failed to send club command: %w", err)
	}

	// Send detect ball command
	seq = lm.getNextSequence()
	detectCommand := DetectBallCommand(seq, Activate, *spinMode)

	err = lm.SendCommand(detectCommand)
	if err != nil {
		return fmt.Errorf("failed to send detect ball command: %w", err)
	}

	return nil
}

// rearmDetectionOnly sends just the DetectBall command, leaving the
// device's currently-selected club untouched. Used by the post-shot
// auto-rearm path to avoid disrupting per-club detection tuning.
//
// Why this matters: when a putter is selected, the device tunes its
// sensors for low-energy shots. Re-sending the club command on every
// shot — as the previous reactivateBallDetection did — appears to
// briefly reset that tuning and cause the next short putt to be
// missed entirely (no 0x11 0x02 ball-metrics notification at all).
// The official Square app and upstream SquareConnector handle short
// putts cleanly; sending only the DetectBall arm is consistent with
// what the device actually needs to keep listening.
func (lm *LaunchMonitor) rearmDetectionOnly() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	spinMode := lm.stateManager.GetSpinMode()
	if spinMode == nil {
		defaultSpinMode := Advanced
		spinMode = &defaultSpinMode
	}

	seq := lm.getNextSequence()
	detectCommand := DetectBallCommand(seq, Activate, *spinMode)
	if err := lm.SendCommand(detectCommand); err != nil {
		return fmt.Errorf("failed to send detect ball command: %w", err)
	}
	return nil
}

// DeactivateBallDetection deactivates ball detection mode
func (lm *LaunchMonitor) DeactivateBallDetection() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	// Get current spin mode from state
	spinMode := lm.stateManager.GetSpinMode()
	if spinMode == nil {
		defaultSpinMode := Advanced
		spinMode = &defaultSpinMode
	}

	seq := lm.getNextSequence()
	detectCommand := DetectBallCommand(seq, Deactivate, *spinMode)

	err := lm.SendCommand(detectCommand)
	if err != nil {
		return fmt.Errorf("failed to send detect ball command: %w", err)
	}

	return nil
}

// reactivateBallDetection re-activates ball detection after a shot completes
// (internal callers within the core package).
func (lm *LaunchMonitor) reactivateBallDetection() {
	lm.ReactivateBallDetectionFromSource("club-metrics")
}

// ReactivateBallDetectionFromSource is the single re-arm entry point used by
// every post-shot path (club-metrics callback, GSPro ack, fallback timer).
// It applies an 800 ms coalescing window so that overlapping triggers don't
// fire two BLE arming sequences back-to-back — a race we believe was
// preventing the device from picking up a second short putt taken quickly
// after the first.
//
// `source` is logged so we can tell which path actually fired in the
// remaining call (and which were coalesced away).
func (lm *LaunchMonitor) ReactivateBallDetectionFromSource(source string) {
	lm.rearmMu.Lock()
	now := time.Now()
	if !lm.lastRearmAt.IsZero() && now.Sub(lm.lastRearmAt) < rearmCoalesceWindow {
		elapsed := now.Sub(lm.lastRearmAt)
		lm.rearmMu.Unlock()
		log.Printf("LaunchMonitor: Re-arm from %s coalesced (%.0fms since last)", source, float64(elapsed)/float64(time.Millisecond))
		return
	}
	lm.lastRearmAt = now
	lm.rearmMu.Unlock()

	go func() {
		// Small delay to let the device finish processing the shot
		time.Sleep(500 * time.Millisecond)

		if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
			log.Printf("LaunchMonitor: Skip re-arm from %s — not connected", source)
			return
		}

		// Use the lightweight re-arm: only sends DetectBall, doesn't
		// re-send the club command. See rearmDetectionOnly() for why.
		log.Printf("LaunchMonitor: Re-arming detection (source=%s)", source)
		err := lm.rearmDetectionOnly()
		if err != nil {
			log.Printf("LaunchMonitor: Failed to re-arm detection (source=%s): %v", source, err)
		}
	}()
}

// Helper functions

// getNextSequence gets the next sequence number with thread safety
func (lm *LaunchMonitor) getNextSequence() int {
	lm.sequenceMutex.Lock()
	defer lm.sequenceMutex.Unlock()

	seq := lm.sequence
	lm.sequence++
	if lm.sequence > 255 {
		lm.sequence = 0
	}
	return seq
}

// startHeartbeatTask starts the heartbeat task to maintain device connection
func (lm *LaunchMonitor) startHeartbeatTask() {
	lm.heartbeatCancelMu.Lock()
	defer lm.heartbeatCancelMu.Unlock()

	// Cancel any existing heartbeat task
	if lm.heartbeatCancel != nil {
		lm.heartbeatCancel()
		lm.heartbeatCancel = nil
	}

	// Create a new context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	lm.heartbeatCancel = cancel

	// Start the heartbeat task in a goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if lm.bluetoothClient != nil && lm.bluetoothClient.IsConnected() {
					seq := lm.getNextSequence()
					command := HeartbeatCommand(seq)
					err := lm.SendCommand(command)
					if err != nil {
						log.Printf("Error sending heartbeat: %v", err)
					}
				}
			}
		}
	}()
}

// ManageHeartbeat initializes and manages the heartbeat communication with the device
func (lm *LaunchMonitor) ManageHeartbeat() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	// Start the heartbeat task
	lm.startHeartbeatTask()

	// Send initial heartbeat
	seq := lm.getNextSequence()
	heartbeatCommand := HeartbeatCommand(seq)
	err := lm.SendCommand(heartbeatCommand)
	if err != nil {
		return fmt.Errorf("failed to send initial heartbeat: %w", err)
	}

	return nil
}

// SetupNotifications registers the launch monitor's notification handler with the Bluetooth manager
func (lm *LaunchMonitor) SetupNotifications(btManager *BluetoothManager) {
	// Create a closure that adapts the LaunchMonitor's NotificationHandler to match
	// what BluetoothManager expects, while still providing the BluetoothClient
	btManager.SetNotificationHandler(func(uuid string, data []byte) {
		// Call the LaunchMonitor's NotificationHandler with the client
		lm.NotificationHandler(uuid, data)
	})

	// Register pre-disconnect hook to try to deactivate ball detection before disconnection
	btManager.SetPreDisconnectHook(func() {
		if lm.bluetoothClient != nil && lm.bluetoothClient.IsConnected() {
			log.Println("LaunchMonitor: Attempting to deactivate ball detection before disconnection")
			err := lm.DeactivateBallDetection()
			if err != nil {
				log.Printf("LaunchMonitor: Failed to deactivate ball detection: %v", err)
			} else {
				log.Println("LaunchMonitor: Successfully deactivated ball detection")
			}
		}
	})

	// Register for connection status changes to handle disconnects and connection setup
	lm.stateManager.RegisterConnectionStatusCallback(func(oldValue, newValue ConnectionStatus) {
		if newValue == ConnectionStatusConnected && oldValue != ConnectionStatusConnected {
			// Note: Firmware version is not available via BLE
			// The device does not respond to the 0x92 command
			// The Android app does not request it either
			log.Println("LaunchMonitor: Device connected (firmware version not available via BLE)")
		} else if newValue == ConnectionStatusDisconnected {
			// When Bluetooth disconnects, reset ball detection state
			lm.HandleBluetoothDisconnect()
		}
	})

	// Start the heartbeat task to maintain connection
	lm.startHeartbeatTask()
}

// HandleBluetoothDisconnect handles cleanup when Bluetooth disconnects
func (lm *LaunchMonitor) HandleBluetoothDisconnect() {
	log.Println("LaunchMonitor: Bluetooth disconnected - resetting ball detection state")

	// Reset ball detection state in the state manager
	lm.stateManager.SetBallDetected(false)
	lm.stateManager.SetBallReady(false)
	lm.stateManager.SetBallPosition(nil)

	// Stop any heartbeat task
	lm.heartbeatCancelMu.Lock()
	if lm.heartbeatCancel != nil {
		lm.heartbeatCancel()
		lm.heartbeatCancel = nil
	}
	lm.heartbeatCancelMu.Unlock()
}

// StartAlignment starts alignment mode
func (lm *LaunchMonitor) StartAlignment() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	// Get handedness, default to RightHanded if not set
	handednessPtr := lm.stateManager.GetHandedness()
	handedness := RightHanded
	if handednessPtr != nil {
		handedness = *handednessPtr
	}

	// Check if already in alignment mode
	// If so, only send commands if this is NOT a duplicate call from navigation
	// We can tell it's a handedness change request because the frontend always
	// updates handedness state before calling StartAlignment
	if lm.stateManager.GetIsAligning() {
		// Already aligning - this is likely just navigation returning to the screen
		// Don't send duplicate commands
		return nil
	}

	// First, send the alignment-stick club command (clubSel=0x08,
	// type=0x08). This puts the device in alignment mode.
	seq := lm.getNextSequence()

	command := AlignmentStickCommand(seq, handedness)
	err := lm.SendCommand(command)
	if err != nil {
		return fmt.Errorf("failed to start alignment: %w", err)
	}

	time.Sleep(1 * time.Second)

	// Activate ball detection mode 2 to turn on the red LED
	detectSeq := lm.getNextSequence()
	detectCmd := DetectBallCommand(detectSeq, ActivateAlignmentMode, Advanced)
	err = lm.SendCommand(detectCmd)
	if err != nil {
		return fmt.Errorf("failed to activate ball detection: %w", err)
	}

	time.Sleep(100 * time.Millisecond)

	lm.stateManager.SetIsAligning(true)
	return nil
}

// StopAlignment stops alignment mode and saves calibration (OK button)
func (lm *LaunchMonitor) StopAlignment() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	// Get current alignment angle to send as target
	currentAngle := lm.stateManager.GetAlignmentAngle()

	// Send stop alignment command (confirm=1, current angle)
	seq := lm.getNextSequence()
	command := StopAlignmentCommand(seq, currentAngle)
	err := lm.SendCommand(command)
	if err != nil {
		return fmt.Errorf("failed to stop alignment: %w", err)
	}

	// Update state
	lm.stateManager.SetIsAligning(false)
	lm.stateManager.SetAlignmentAngle(0)
	lm.stateManager.SetIsAligned(false)
	return nil
}

// CancelAlignment cancels alignment mode without saving calibration (Cancel button)
func (lm *LaunchMonitor) CancelAlignment() error {
	if lm.bluetoothClient == nil || !lm.bluetoothClient.IsConnected() {
		return fmt.Errorf("not connected to device")
	}

	// Get current alignment angle to send with cancel
	currentAngle := lm.stateManager.GetAlignmentAngle()

	// Send cancel alignment command (confirm=0, current angle)
	seq := lm.getNextSequence()
	command := CancelAlignmentCommand(seq, currentAngle)
	err := lm.SendCommand(command)
	if err != nil {
		return fmt.Errorf("failed to cancel alignment: %w", err)
	}

	// Update state
	lm.stateManager.SetIsAligning(false)
	lm.stateManager.SetAlignmentAngle(0)
	lm.stateManager.SetIsAligned(false)
	return nil
}

// RequestFirmwareVersion requests the device firmware version
func (lm *LaunchMonitor) RequestFirmwareVersion() error {
	log.Printf("LaunchMonitor: RequestFirmwareVersion called")

	if lm.bluetoothClient == nil {
		log.Printf("LaunchMonitor: bluetoothClient is nil")
		return fmt.Errorf("bluetoothClient is nil")
	}

	if !lm.bluetoothClient.IsConnected() {
		log.Printf("LaunchMonitor: device not connected")
		return fmt.Errorf("not connected to device")
	}

	seq := lm.getNextSequence()
	command := GetOSVersionCommand(seq)
	log.Printf("LaunchMonitor: Sending firmware version request command: %v", command)

	err := lm.SendCommand(command)
	if err != nil {
		log.Printf("LaunchMonitor: Failed to send firmware version command: %v", err)
		return fmt.Errorf("failed to request firmware version: %w", err)
	}

	log.Printf("LaunchMonitor: Firmware version request sent successfully")
	return nil
}

// HandleOSVersionNotification handles OS version response notifications (format 11 10)
func (lm *LaunchMonitor) HandleOSVersionNotification(bytesList []string) {
	// Format: 11 10 {major} {minor}
	// Example: 11 10 01 09 = version 1.9
	// The bytes are hex strings representing decimal values
	log.Printf("Raw OS version bytes: %v (len=%d)", bytesList, len(bytesList))

	if len(bytesList) < 4 {
		log.Printf("Invalid OS version notification format, expected at least 4 bytes, got %d", len(bytesList))
		return
	}

	// Parse hex strings as hex values to get decimal
	// bytesList[2] is major version (hex string like "01" = decimal 1)
	// bytesList[3] is minor version (hex string like "09" = decimal 9)
	major, err := strconv.ParseInt(bytesList[2], 16, 64)
	if err != nil {
		log.Printf("Error parsing major version from '%s': %v", bytesList[2], err)
		return
	}

	minor, err := strconv.ParseInt(bytesList[3], 16, 64)
	if err != nil {
		log.Printf("Error parsing minor version from '%s': %v", bytesList[3], err)
		return
	}

	version := fmt.Sprintf("%d.%d", major, minor)

	log.Printf("Device firmware version: %s (major=%d, minor=%d)", version, major, minor)

	// Update state manager
	lm.stateManager.SetFirmwareVersion(&version)
}

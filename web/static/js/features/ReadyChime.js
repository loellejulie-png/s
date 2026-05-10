// features/ReadyChime.js
//
// Short two-note ascending tone played when the device reports the
// ball is in the hitting zone and ready to detect. Lets the player
// hear when they're set up correctly without glancing at the device.
//
// Synthesized via the Web Audio API so we don't ship an audio asset
// (keeps the bundle small and avoids an extra file in the embedded
// fs.FS).
//
// Caller responsibilities:
//   - Call ReadyChime.setEnabled(bool) when the user toggles the
//     "Audio cues" setting.
//   - Call ReadyChime.onBallReadyState(ballReady, ballDetected) on
//     every device-status update. Internal logic handles rising-edge
//     detection and debounce so a flickering sensor doesn't fire a
//     stream of chimes.

let enabled = true;
let ctx = null;

// Rising-edge bookkeeping
let chimedForThisPlacement = false;
let rearmTimer = null;
const REARM_DEBOUNCE_MS = 1500;

function audioContext() {
    if (ctx) return ctx;
    try {
        const Ctor = window.AudioContext || window.webkitAudioContext;
        if (!Ctor) return null;
        ctx = new Ctor();
    } catch (_) {
        ctx = null;
    }
    return ctx;
}

function playNote(context, frequency, startAt, duration, gain) {
    const osc = context.createOscillator();
    const env = context.createGain();
    osc.type = 'sine';
    osc.frequency.setValueAtTime(frequency, startAt);

    // Soft attack/decay so it doesn't click.
    env.gain.setValueAtTime(0, startAt);
    env.gain.linearRampToValueAtTime(gain, startAt + 0.015);
    env.gain.exponentialRampToValueAtTime(0.0001, startAt + duration);

    osc.connect(env);
    env.connect(context.destination);
    osc.start(startAt);
    osc.stop(startAt + duration + 0.05);
}

function play() {
    if (!enabled) return;
    const c = audioContext();
    if (!c) return;

    const fire = () => {
        const t = c.currentTime + 0.01;
        // Ascending two-note chime: G5 → C6.
        playNote(c, 783.99, t,        0.18, 0.18);
        playNote(c, 1046.5, t + 0.13, 0.22, 0.20);
    };

    // Browsers gate AudioContext on a user gesture; the first call
    // after page load may have a suspended context.
    if (c.state === 'suspended') {
        c.resume().then(fire).catch(() => { /* ignore */ });
    } else {
        fire();
    }
}

export const ReadyChime = {
    setEnabled(value) {
        enabled = !!value;
    },

    isEnabled() {
        return enabled;
    },

    /**
     * Drive the chime from a deviceStatus update. Fires once per ball
     * placement and re-arms only after the ball has been undetected
     * continuously for REARM_DEBOUNCE_MS — long enough to ignore
     * sensor flicker around the detection threshold, short enough that
     * the next legitimate placement re-chimes.
     *
     * @param {boolean} ballReady   - device-reported ready state
     * @param {boolean} ballDetected - device-reported detection state
     */
    onBallReadyState(ballReady, ballDetected) {
        if (ballReady && !chimedForThisPlacement) {
            play();
            chimedForThisPlacement = true;
        }

        if (ballDetected) {
            // Detection back — cancel any pending re-arm.
            if (rearmTimer) {
                clearTimeout(rearmTimer);
                rearmTimer = null;
            }
        } else if (chimedForThisPlacement && !rearmTimer) {
            // Sustained absence → schedule re-arm after debounce.
            rearmTimer = setTimeout(() => {
                chimedForThisPlacement = false;
                rearmTimer = null;
            }, REARM_DEBOUNCE_MS);
        }
    },

    /**
     * Reset internal state. Call when the device disconnects so the
     * next reconnect-and-place sequence chimes correctly.
     */
    reset() {
        chimedForThisPlacement = false;
        if (rearmTimer) {
            clearTimeout(rearmTimer);
            rearmTimer = null;
        }
    }
};

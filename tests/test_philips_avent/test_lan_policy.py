"""Tests for the LAN keep-alive and reconnect policy (issue #62)."""

from lan_policy import (
    DATA_TIMEOUT,
    HEARTBEAT_INTERVAL,
    LAN_ERR_CONNECT,
    LAN_ERR_OFFLINE,
    LAN_ERR_PAYLOAD,
    MAX_PAYLOAD_ERRORS,
    RECONNECT_DELAY,
    data_stale,
    heartbeat_due,
    is_connection_error,
    reconnect_delay,
    should_reconnect,
)

# tinytuya blocks in receive() for up to this long, so a heartbeat can be late
# by that much. Mirrors SOCKET_TIMEOUT in lan.py.
SOCKET_TIMEOUT = 5

# The monitor closes an idle socket after this long at the earliest.
DEVICE_IDLE_CLOSE = 30


class TestHeartbeatInterval:
    def test_ping_lands_well_inside_the_device_idle_window(self):
        # Worst case: a heartbeat becomes due right after a receive started.
        assert HEARTBEAT_INTERVAL + SOCKET_TIMEOUT < DEVICE_IDLE_CLOSE

    def test_due_only_after_the_interval(self):
        assert not heartbeat_due(now=100.0, last_heartbeat=100.0)
        assert not heartbeat_due(now=100.0 + HEARTBEAT_INTERVAL - 0.1, last_heartbeat=100.0)
        assert heartbeat_due(now=100.0 + HEARTBEAT_INTERVAL, last_heartbeat=100.0)
        assert heartbeat_due(now=200.0, last_heartbeat=100.0)

    def test_interval_is_overridable(self):
        assert heartbeat_due(now=5.0, last_heartbeat=0.0, interval=4.0)
        assert not heartbeat_due(now=5.0, last_heartbeat=0.0, interval=6.0)


class TestDataWatchdog:
    def test_stale_only_after_the_timeout(self):
        assert not data_stale(now=100.0, last_data=100.0)
        assert not data_stale(now=100.0 + DATA_TIMEOUT, last_data=100.0)
        assert data_stale(now=100.0 + DATA_TIMEOUT + 0.1, last_data=100.0)

    def test_answered_heartbeats_keep_the_watchdog_quiet(self):
        # Heartbeats refresh last_data, so on a healthy connection the watchdog
        # never fires even when the monitor pushes no DPS updates.
        last_data = 0.0
        for tick in range(1, 500):
            now = tick * HEARTBEAT_INTERVAL
            assert not data_stale(now, last_data)
            last_data = now


class TestReconnectDelay:
    def test_first_failure_retries_immediately(self):
        # A device that just closed the socket is usually ready again at once,
        # and waiting keeps the camera on cloud polling for longer than needed.
        assert reconnect_delay(1) == 0.0

    def test_later_failures_back_off(self):
        assert reconnect_delay(2) == RECONNECT_DELAY
        assert reconnect_delay(7) == RECONNECT_DELAY

    def test_zero_or_negative_is_treated_as_first(self):
        assert reconnect_delay(0) == 0.0


class TestErrorClassification:
    def test_connection_codes(self):
        assert is_connection_error(LAN_ERR_CONNECT)
        assert is_connection_error(LAN_ERR_OFFLINE)

    def test_payload_code_is_not_a_connection_error(self):
        # "Unexpected Payload from Device" (904) is what the monitor emitted
        # every 30-45s in issue #62; it says nothing about the socket.
        assert not is_connection_error(LAN_ERR_PAYLOAD)

    def test_missing_or_unknown_codes(self):
        assert not is_connection_error(None)
        assert not is_connection_error("")
        assert not is_connection_error("999")

    def test_codes_accepted_as_int_or_str(self):
        assert is_connection_error(int(LAN_ERR_OFFLINE))


class TestShouldReconnect:
    def test_connection_error_reconnects_at_once(self):
        assert should_reconnect(LAN_ERR_OFFLINE, 1)
        assert should_reconnect(LAN_ERR_CONNECT, 1)

    def test_single_unreadable_frame_does_not_reconnect(self):
        assert not should_reconnect(LAN_ERR_PAYLOAD, 1)

    def test_repeated_unreadable_frames_do_reconnect(self):
        assert not should_reconnect(LAN_ERR_PAYLOAD, MAX_PAYLOAD_ERRORS - 1)
        assert should_reconnect(LAN_ERR_PAYLOAD, MAX_PAYLOAD_ERRORS)

    def test_error_without_a_code_is_treated_as_unreadable(self):
        assert not should_reconnect(None, 1)
        assert should_reconnect(None, MAX_PAYLOAD_ERRORS)

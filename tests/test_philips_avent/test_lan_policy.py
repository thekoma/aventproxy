"""Tests for the LAN keep-alive and reconnect policy (issue #62)."""

from lan_policy import (
    DATA_TIMEOUT,
    HEARTBEAT_INTERVAL,
    KEYED_PROTOCOL_MIN_VERSION,
    LAN_ERR_CONNECT,
    LAN_ERR_OFFLINE,
    LAN_ERR_PAYLOAD,
    MAX_PAYLOAD_ERRORS,
    PROTOCOL_VERSION_DEFAULT,
    RECONNECT_DELAY,
    baseline_dps,
    data_stale,
    heartbeat_due,
    heartbeat_outcome,
    is_connection_error,
    keyed_protocol,
    lan_write_delivered,
    parse_protocol_version,
    reconnect_delay,
    seeded_session,
    session_candidates,
    should_reconnect,
    version_candidates,
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


class TestProtocolVersion:
    """Which local protocol version to speak (#51, #62).

    The SCD973/26 announces 3.5 in its discovery broadcast while the client always
    built a 3.3 session, and a session at the wrong version cannot read the
    frames the camera sends.
    """

    def test_announced_version_is_read(self):
        assert parse_protocol_version("3.5") == 3.5
        assert parse_protocol_version("3.3") == 3.3
        assert parse_protocol_version(3.4) == 3.4

    def test_unknown_or_junk_values_fall_back(self):
        for value in ("", "  ", "junk", "9.9", None, True, [], {}):
            assert parse_protocol_version(value) is None

    def test_announced_version_is_tried_first(self):
        assert version_candidates(3.5) == [3.5, PROTOCOL_VERSION_DEFAULT]

    def test_default_alone_when_nothing_announced(self):
        assert version_candidates(None) == [PROTOCOL_VERSION_DEFAULT]

    def test_no_duplicate_when_the_camera_announces_the_default(self):
        assert version_candidates(PROTOCOL_VERSION_DEFAULT) == [PROTOCOL_VERSION_DEFAULT]

    def test_fallback_is_always_available(self):
        # Local control must not regress on firmware that refuses the newer
        # session-key negotiation.
        for announced in (3.1, 3.4, 3.5, None):
            assert PROTOCOL_VERSION_DEFAULT in version_candidates(announced)


class TestHeartbeatOutcome:
    """The keep-alive reads the next frame with a payload, which can be a push (fork 8c4fc03)."""

    def test_an_alert_read_in_place_of_the_ack_is_handed_back(self):
        record = {"dps": {"212": "eyJjbWQiOiJpcGNfbW90aW9uIn0="}, "t": 1}
        assert heartbeat_outcome(record) == (True, {"212": "eyJjbWQiOiJpcGNfbW90aW9uIn0="})

    def test_a_plain_ack_carries_nothing(self):
        assert heartbeat_outcome(None) == (True, None)
        assert heartbeat_outcome({}) == (True, None)
        assert heartbeat_outcome({"dps": {}}) == (True, None)

    def test_a_connection_error_means_the_socket_is_gone(self):
        assert heartbeat_outcome({"Error": "Network Error", "Err": LAN_ERR_CONNECT}) == (False, None)
        assert heartbeat_outcome({"Error": "Offline", "Err": LAN_ERR_OFFLINE}) == (False, None)

    def test_an_unreadable_ack_still_proves_the_socket_is_alive(self):
        assert heartbeat_outcome({"Error": "Unexpected Payload", "Err": LAN_ERR_PAYLOAD}) == (True, None)

    def test_junk_dps_is_not_published(self):
        assert heartbeat_outcome({"dps": "not a dict"}) == (True, None)


class TestLanWriteDelivered:
    """tinytuya returns a failed write as an error dict; the cloud mirror used to hide it."""

    def test_a_reply_or_no_reply_counts_as_delivered(self):
        assert lan_write_delivered(None)
        assert lan_write_delivered({"dps": {"138": True}, "t": 1})

    def test_no_socket_means_the_command_never_left(self):
        assert not lan_write_delivered({"Error": "Network Error: Device Unreachable", "Err": LAN_ERR_OFFLINE})
        assert not lan_write_delivered({"Error": "Network Error: Unable to Connect", "Err": LAN_ERR_CONNECT})
        assert not lan_write_delivered({"Error": "Check device key or version", "Err": "914"})

    def test_a_late_or_unreadable_reply_is_not_sent_twice(self):
        # A repeated relative step would advance the lullaby twice (issue #91).
        assert lan_write_delivered({"Error": "Timeout Waiting for Device", "Err": "902"})
        assert lan_write_delivered({"Error": "Unexpected Payload from Device", "Err": LAN_ERR_PAYLOAD})


class TestKeyedProtocol:
    """What a session carries on its own depends on the protocol it negotiated (fork c3d0b83, 619981b)."""

    def test_3_4_and_up_are_keyed(self):
        assert KEYED_PROTOCOL_MIN_VERSION == 3.4
        assert keyed_protocol(3.4)
        assert keyed_protocol(3.5)

    def test_3_3_keeps_todays_behaviour(self):
        # The maintainer's SCD973 on firmware 1.4.0 speaks 3.3.
        assert not keyed_protocol(3.3)
        assert not keyed_protocol(PROTOCOL_VERSION_DEFAULT)
        assert not keyed_protocol(3.1)

    def test_no_session_is_not_keyed(self):
        assert not keyed_protocol(None)


class TestBaselineDps:
    """A DP_QUERY answer becomes the baseline; anything else leaves the session alone."""

    def test_the_answer_is_the_state(self):
        assert baseline_dps({"dps": {"207": 2570, "158": 70}, "t": 1}) == {"207": 2570, "158": 70}

    def test_an_error_frame_carries_no_state(self):
        assert baseline_dps({"Error": "Timeout Waiting for Device", "Err": "902"}) is None
        assert baseline_dps({"Error": "x", "Err": LAN_ERR_PAYLOAD, "dps": {"207": 1}}) is None

    def test_nothing_or_junk_carries_no_state(self):
        for result in (None, {}, {"dps": {}}, {"dps": "x"}, "x"):
            assert baseline_dps(result) is None


class TestSeededSession:
    """A restart may dial the last published pair before scanning, when a handshake can prove it."""

    def test_a_keyed_pair_is_dialled_first(self):
        assert seeded_session("192.168.1.9", "3.5") == ("192.168.1.9", 3.5)
        assert seeded_session("192.168.1.9", "3.4") == ("192.168.1.9", 3.4)

    def test_a_3_3_pair_still_scans_first(self):
        # A 3.3 connect is bare TCP and proves neither the address nor the version.
        assert seeded_session("192.168.1.9", "3.3") == (None, None)

    def test_nothing_published_means_a_scan(self):
        assert seeded_session(None, "") == (None, None)
        assert seeded_session("", "3.5") == (None, None)
        assert seeded_session("192.168.1.9", "") == (None, None)
        assert seeded_session("192.168.1.9", "junk") == (None, None)


class TestSessionCandidates:
    """Which versions a (re)connect tries, and in what order."""

    def test_a_3_3_monitor_is_unchanged(self):
        # The maintainer's SCD973 on firmware 1.4.0: only ever 3.3.
        for scanned in (True, False):
            assert session_candidates(3.3, None, scanned=scanned) == [3.3]
            assert session_candidates(3.3, 3.3, scanned=scanned) == [3.3]
            assert session_candidates(None, 3.3, scanned=scanned) == [3.3]
            assert session_candidates(None, None, scanned=scanned) == [PROTOCOL_VERSION_DEFAULT]

    def test_after_a_scan_the_fallback_stays(self):
        assert session_candidates(3.5, None, scanned=True) == [3.5, PROTOCOL_VERSION_DEFAULT]

    def test_a_fallback_session_does_not_pin_the_monitor_to_3_3(self):
        # One unconfirmed 3.3 session used to make every later reconnect start at 3.3.
        assert session_candidates(3.5, 3.3, scanned=True) == [3.5, PROTOCOL_VERSION_DEFAULT]
        assert session_candidates(3.5, 3.3, scanned=False) == [3.5]

    def test_a_cached_address_is_trusted_only_through_a_handshake(self):
        # A failed handshake there means a rescan, not a bare TCP session with
        # whatever now answers at that address.
        assert session_candidates(3.5, 3.5, scanned=False) == [3.5]
        assert session_candidates(3.4, None, scanned=False) == [3.4]

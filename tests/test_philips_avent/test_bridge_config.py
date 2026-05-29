"""Tests for the bridge-config camera payload builder.

Imports the real ``build_cameras_payload`` so a contract drift (e.g. someone
changing it to omit product_id) is caught here, not just in the Go integration
tests.

The import below uses the leaf form ``from payload import ...`` to match the
project convention (see ``conftest.py``): the full path
``custom_components.philips_avent.payload`` cannot be used because it triggers
``custom_components/philips_avent/__init__.py``, which imports ``homeassistant``
— a runtime dependency we deliberately keep out of unit tests.
"""

from payload import build_cameras_payload


class TestBuildCamerasPayloadShape:
    def test_product_id_propagated_for_scd951(self):
        cameras = [
            {"deviceId": "abc123", "deviceName": "Baby", "productId": "selj2idknqhjnids"},
        ]
        payload = build_cameras_payload(cameras)
        assert len(payload) == 1
        assert payload[0] == {
            "camera_id": "abc123",
            "camera_name": "Baby",
            "product_id": "selj2idknqhjnids",
        }

    def test_product_id_empty_for_scd973(self):
        """No regression for devices without a productId — empty string passes through."""
        cameras = [{"deviceId": "def456", "deviceName": "Cam"}]
        assert build_cameras_payload(cameras)[0]["product_id"] == ""

    def test_mixed_cameras(self):
        cameras = [
            {"deviceId": "abc123", "deviceName": "Baby SCD951", "productId": "selj2idknqhjnids"},
            {"deviceId": "def456", "deviceName": "Baby SCD973", "productId": ""},
        ]
        payload = build_cameras_payload(cameras)
        assert payload[0]["product_id"] == "selj2idknqhjnids"
        assert payload[1]["product_id"] == ""

    def test_devid_fallback_key(self):
        """Raw Tuya discovery dicts use 'devId' not 'deviceId'."""
        cameras = [{"devId": "xyz789", "deviceName": "Cam", "productId": "someproduct"}]
        assert build_cameras_payload(cameras)[0]["camera_id"] == "xyz789"

    def test_product_key_fallback(self):
        """Some Tuya endpoints return 'productKey' instead of 'productId'."""
        cameras = [{"deviceId": "abc", "deviceName": "Cam", "productKey": "alt-product-key"}]
        assert build_cameras_payload(cameras)[0]["product_id"] == "alt-product-key"

    def test_snake_case_product_id_fallback(self):
        """A stored-entry dict (pre-conversion) uses 'product_id'."""
        cameras = [{"id": "abc", "name": "Cam", "product_id": "stored-pid"}]
        payload = build_cameras_payload(cameras)
        assert payload[0]["camera_id"] == "abc"
        assert payload[0]["camera_name"] == "Cam"
        assert payload[0]["product_id"] == "stored-pid"

    def test_name_fallback_to_default(self):
        """A camera with no name at all gets the literal default 'camera'."""
        cameras = [{"deviceId": "abc"}]
        assert build_cameras_payload(cameras)[0]["camera_name"] == "camera"

    def test_empty_input(self):
        assert build_cameras_payload([]) == []


class TestBuildCamerasPayloadPrecedence:
    """When multiple keys are present, the documented precedence applies."""

    def test_device_id_wins_over_devid(self):
        cameras = [{"deviceId": "first", "devId": "second"}]
        assert build_cameras_payload(cameras)[0]["camera_id"] == "first"

    def test_device_name_wins_over_name(self):
        cameras = [{"deviceId": "abc", "deviceName": "first", "name": "second"}]
        assert build_cameras_payload(cameras)[0]["camera_name"] == "first"

    def test_product_id_wins_over_product_key(self):
        cameras = [
            {"deviceId": "abc", "deviceName": "Cam", "productId": "id-form", "productKey": "key-form"},
        ]
        assert build_cameras_payload(cameras)[0]["product_id"] == "id-form"

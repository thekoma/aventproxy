"""Tests for the shared device_info builder (issue #42: wrong hard-coded model)."""
from types import SimpleNamespace

from entity import build_device_info
from const import DOMAIN, PRODUCT_ID_TO_MODEL


def _coordinator(device_info, name="Nursery"):
    return SimpleNamespace(camera_name=name, device_info=device_info)


class TestBuildDeviceInfo:
    def test_generic_model_for_unknown_product_id(self):
        """No more hard-coded SCD973: unknown productId gets the generic model."""
        info = build_device_info(_coordinator({"productId": "selj2idknqhjnids"}), "cam1")
        assert info["model"] == "Avent Baby Monitor"

    def test_product_id_exposed_as_model_id(self):
        info = build_device_info(_coordinator({"productId": "selj2idknqhjnids"}), "cam1")
        assert info["model_id"] == "selj2idknqhjnids"

    def test_no_product_id_omits_model_id(self):
        info = build_device_info(_coordinator({}), "cam1")
        assert info["model"] == "Avent Baby Monitor"
        assert info.get("model_id") is None

    def test_identifiers_name_manufacturer(self):
        info = build_device_info(_coordinator({}, name="Erik"), "bf3ca1e955a0361563hdwr")
        assert info["identifiers"] == {(DOMAIN, "bf3ca1e955a0361563hdwr")}
        assert info["name"] == "Erik"
        assert info["manufacturer"] == "Philips"

    def test_mapped_product_id_wins_over_generic(self):
        """When a productId is confirmed to identify a model, the map takes over."""
        PRODUCT_ID_TO_MODEL["kx9f2mchbq7wetuz"] = "Avent SCD953"
        try:
            info = build_device_info(_coordinator({"productId": "kx9f2mchbq7wetuz"}), "cam1")
            assert info["model"] == "Avent SCD953"
        finally:
            del PRODUCT_ID_TO_MODEL["kx9f2mchbq7wetuz"]

    def test_none_device_info_is_tolerated(self):
        """Coordinator.device_info could be reset to None; don't crash."""
        info = build_device_info(_coordinator(None), "cam1")
        assert info["model"] == "Avent Baby Monitor"

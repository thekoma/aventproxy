"""Shared entity helpers for Philips Avent platforms."""
from __future__ import annotations

try:
    from .const import DEFAULT_MODEL, DOMAIN, PRODUCT_ID_TO_MODEL
except ImportError:  # loaded standalone by unit tests, without the HA package
    from const import DEFAULT_MODEL, DOMAIN, PRODUCT_ID_TO_MODEL


def build_device_info(coordinator, cam_id: str) -> dict:
    """Device registry info shared by every platform.

    The model falls back to a family-generic name because the Tuya productId
    is shared across SCD9xx variants and cannot identify the exact model
    (issue #42). The raw productId is still surfaced as ``model_id`` so it
    shows up in the device page and in diagnostics.
    """
    product_id = (coordinator.device_info or {}).get("productId") or ""
    info = {
        "identifiers": {(DOMAIN, cam_id)},
        "name": coordinator.camera_name,
        "manufacturer": "Philips",
        "model": PRODUCT_ID_TO_MODEL.get(product_id, DEFAULT_MODEL),
    }
    if product_id:
        info["model_id"] = product_id
    return info

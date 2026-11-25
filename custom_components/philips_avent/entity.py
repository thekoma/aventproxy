"""Shared entity helpers for Philips Avent platforms."""
from __future__ import annotations

import logging
from collections.abc import Callable, Iterable
from typing import Any

try:
    from .const import DEFAULT_MODEL, DOMAIN, PRODUCT_ID_TO_MODEL
except ImportError:  # loaded standalone by unit tests, without the HA package
    from const import DEFAULT_MODEL, DOMAIN, PRODUCT_ID_TO_MODEL

_LOGGER = logging.getLogger(__name__)

# (ready, build): ready(dps) says whether the data is there, build() makes the entities.
EntityGroup = tuple[Callable[[dict], bool], Callable[[], list]]


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
        "name": coordinator.display_name,
        "manufacturer": "Philips",
        "model": PRODUCT_ID_TO_MODEL.get(product_id, DEFAULT_MODEL),
    }
    if product_id:
        info["model_id"] = product_id
    return info


def dps_present(dps_id: str) -> Callable[[dict], bool]:
    """A readiness check for add_entities_when_ready: the data point exists."""
    return lambda dps: dps_id in dps


def add_entities_when_ready(
    coordinator: Any,
    async_add_entities: Callable[[list], Any],
    groups: Iterable[EntityGroup],
    on_unload: Callable[[Callable[[], None]], Any],
) -> None:
    """Create each group of entities the first time its data is there.

    `groups` holds `(ready, build)` pairs: `ready(dps)` decides from the
    coordinator data, `build()` returns the entities. Each group is added at
    most once. The check runs now and again on every coordinator update,
    because a data point can first show up after setup, as a LAN push or on a
    later cloud poll, and deciding at startup alone would leave the monitor
    without those entities until the next restart (issue #74).

    The check runs inside the coordinator's listener dispatch, which a LAN push
    triggers, so a group that fails to build is logged and dropped rather than
    raising into it, or retried on every push.
    """
    pending = list(groups)

    def _check() -> None:
        if not pending:
            return
        dps = coordinator.data or {}
        entities = []
        for group in [group for group in pending if group[0](dps)]:
            pending.remove(group)
            try:
                entities.extend(group[1]())
            except Exception:  # callback boundary inside the coordinator's listener loop
                _LOGGER.exception("Could not create entities for %s", getattr(coordinator, "name", coordinator))
        if entities:
            async_add_entities(entities)

    _check()
    if pending:
        on_unload(coordinator.async_add_listener(_check))

"""Button entities for Philips Avent Baby Monitor."""

from homeassistant.components.button import ButtonEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import DOMAIN, DPS_LULLABY_CONTROL
from .coordinator import PhilipsAventCoordinator


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    entities = []
    for cam_id, coordinator in data["coordinators"].items():
        for action, icon in [
            ("play", "mdi:play"),
            ("pause", "mdi:pause"),
            ("stop", "mdi:stop"),
            ("next", "mdi:skip-next"),
            ("prev", "mdi:skip-previous"),
        ]:
            entities.append(AventLullabyButton(coordinator, cam_id, action, icon))
    async_add_entities(entities)


class AventLullabyButton(CoordinatorEntity, ButtonEntity):
    _attr_has_entity_name = True

    def __init__(
        self, coordinator: PhilipsAventCoordinator, cam_id: str,
        action: str, icon: str,
    ):
        super().__init__(coordinator)
        self._cam_id = cam_id
        self._action = action
        self._attr_name = f"Lullaby {action.title()}"
        self._attr_icon = icon
        self._attr_unique_id = f"{cam_id}_lullaby_{action}"
        self._attr_device_info = {
            "identifiers": {(DOMAIN, cam_id)},
            "name": coordinator.camera_name,
            "manufacturer": "Philips",
            "model": "Avent SCD973",
        }

    async def async_press(self) -> None:
        await self.coordinator.set_dps({DPS_LULLABY_CONTROL: self._action})

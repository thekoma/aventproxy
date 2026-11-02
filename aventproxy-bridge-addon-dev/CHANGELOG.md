# Changelog

This is the dev card: it follows builds off main, made for testing before an rc.
Stay on the stable card unless you are testing, and use the beta card if you only want release candidates.

## Unreleased (dev2)

The bridge now uses the LAN only for monitors that speak Tuya LAN protocol 3.5. Monitors on protocol 3.3, such as an SCD973 on firmware 1.4.0, keep streaming over the cloud without a 4-second detour and without probing the LAN every 5 minutes.

## Unreleased (dev)

Streams now go over your local network when the monitor allows it, and fall back to the cloud when it does not. This is Leonard Pitzu's work (#51), ported and hardened. It has only been tested on an SCD973/26. If your stream stopped working after this update, turn off **LAN streaming** in the integration options and open an issue with your monitor model and the bridge log.

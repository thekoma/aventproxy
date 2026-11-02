# Changelog

This is the dev card: it follows builds off main, made for testing before an rc.
Stay on the stable card unless you are testing, and use the beta card if you only want release candidates.

## Unreleased (dev)

Streams now go over your local network when the monitor allows it, and fall back to the cloud when it does not. This is Leonard Pitzu's work (#51), ported and hardened. It has only been tested on an SCD973/26. If your stream stopped working after this update, turn off **LAN streaming** in the integration options and open an issue with your monitor model and the bridge log.

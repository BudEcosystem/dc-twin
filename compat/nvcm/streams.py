"""NVCM 1.4.1's NATS streams, as its chart's nats-box job adds them (deploy/helm/templates/
nautobot.yaml): nv-config-manager (its services' subjects) and nautobot (Nautobot's change
events), file-backed, 50 MB, old messages discarded. Run in the Nautobot container (nats-py).
argv: <nats url> <user> <password>"""
import asyncio
import sys

import nats
from nats.js.api import DiscardPolicy, RetentionPolicy, StorageType, StreamConfig


async def main(url, user, password):
    nc = await nats.connect(url, user=user, password=password)
    js = nc.jetstream()
    for name, subjects in (("nv-config-manager", ["nv-config-manager.nautobotchange", "nv-config-manager.devicechange",
                                                   "nv-config-manager.workflow.result"]),
                           ("nautobot", ["nautobot"])):
        cfg = StreamConfig(name=name, subjects=subjects, retention=RetentionPolicy.LIMITS, storage=StorageType.FILE,
                           num_replicas=1, max_bytes=52428800, discard=DiscardPolicy.OLD, duplicate_window=120)
        try:
            await js.add_stream(cfg)
            print("stream", name, "added")
        except Exception as e:  # an existing stream with this config
            print("stream", name, e)
    await nc.close()


asyncio.run(main(*sys.argv[1:4]))

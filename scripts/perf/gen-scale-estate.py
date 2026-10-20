#!/usr/bin/env python3
"""Writes a scale estate: N generic CPU racks of 20 servers (1,000 server BMCs at N=50), model
switches, twin DHCP. Usage: gen-scale-estate.py [racks] > scale.yaml"""
import sys
n = int(sys.argv[1]) if len(sys.argv) > 1 else 50
print("""apiVersion: dc-twin/v1alpha1
kind: Estate
metadata: { name: scale-%d }
spec:
  seed: 99
  clock: wall
  timeScale: 0.05
  packs: [generic@0.1.0]
  sites:
    - name: site-1
      halls:
        - name: hall-a
          rows:""" % (n * 20))
for row in range((n + 9) // 10):
    print("            - name: row%d\n              positions: 10\n              racks:" % row)
    for pos in range(1, 11):
        r = row * 10 + pos - 1
        if r < n:
            print("                - { name: r%02d, position: %d, format: generic/cpu-rack, params: { servers: 20 } }" % (r, pos))
print("""  networks:
    oob: { role: management, dhcp: twin, cidr: 10.0.0.0/16, fabric: oob }
  fabrics:
    - { name: oob, template: oob-flat, nos: none }
  facility:
    power: { feeds: [{ name: A, capacityW: 9000000 }, { name: B, capacityW: 9000000 }] }
  stacks: []""")

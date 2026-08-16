- **`docs/DIVERGENCES.md` becomes the registry its own header promises** (#413) — Twenty-four
  entries had grown into design essays restating derivations their code comments already carry;
  each is now a row that states the surface, what this platform does, whether that is CONFIRMED
  or INFERRED, and where the reasoning lives. 313 KB → 228 KB with all 142 entries, both section
  headers and every cross-document citation intact. Each row was drafted and then attacked by an
  independent reviewer hunting for claims surviving neither in the row nor anywhere in the repo:
  twenty-five did, and were restored — among them a published `pause_turn` sentence that is the
  turn-classification entry's own strongest counter-evidence, and the strip-not-reject argument
  the 0.2.0 changelog points back here for. Nine accuracy defects were fixed rather than carried
  forward, most inherited from the original entries: an error message named as the only one
  lacking a field path where the code has three, a field the SDK's error union requires on every
  variant presented as ours, a bounded path-id enumeration widened to "every", two Anthropic
  bounds attributed to the MCP protocol, and a cumulative byte ceiling stated as a bound on
  delivered bytes when the code counts only part of what crosses the wire.

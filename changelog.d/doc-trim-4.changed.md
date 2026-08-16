- **The steering docs and two deployment READMEs stop restating what they point at** (#413) —
  [CLAUDE.md](./CLAUDE.md) gains the standing rule this plan was written from: prefer a comment
  beside the code, and let a document earn its place with what spans files. It then follows its
  own rule, dropping the coverage-denominator list, the test-support inventory and the reviewer
  pins in favour of the Makefile recipe, the naming convention and the `run-reviews` skill that
  own them. [README.md](./README.md) stops narrating delivery the changelog already holds — its
  status line says what runs today, and its roadmap lists only what is deferred, which its own
  "progress is tracked in" pointer had always promised. The chart's "Notable values" table and
  the compose stack's variable table were third homes behind `values.yaml`,
  `docker-compose.yml` and `.env.example`, each of which documents every key beside the key;
  both become the shape of the decisions instead. Two gaps closed rather than trimmed: how a
  role-claim *name* is read (a URI-shaped name is one flat key, any other dotted name a path —
  fixed at configuration time so no token can choose the reading), and that a session's MCP
  servers are dialled from the **executor process**, so a firewall around the sandbox does not
  bound them.

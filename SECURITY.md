# Security

Report a vulnerability privately through GitHub: the repository's **Security**
tab, **Report a vulnerability**. Please do not open a public issue for it.

Include what you ran, what happened, and the tare version (`kubectl tare
version`). You will get an answer within a week.

## What is in scope

- `kubectl-tare`. It only reads: the Kubernetes API with your own credentials,
  and Prometheus through the API server's service proxy or `--prometheus`.
  Anything that makes it write to a cluster, run a command, or put a credential
  into its output is a vulnerability.
- The scripts and files it generates: `-o patch`, `-o html`, `diff --write`.
- `agent/`, the proof-of-concept node agent. It runs privileged and serves
  unauthenticated metrics on each node's network by design (see
  [agent/README.md](agent/README.md)); a way to get more than that out of it is
  in scope.

A recommendation that is wrong is a bug, not a vulnerability: open an issue,
and see [KNOWN-FAILURES.md](KNOWN-FAILURES.md) for the ones already found.

# Investigation Protocol

1. **Evidence over assumption**: Never fix what you haven't proven broken
2. **Reproduce the crime**: A bug you cannot reliably replicate does not exist yet
3. **Theory of the crime**: Formulate a strict hypothesis before writing a single line of code
4. **Eliminate suspects**: Isolate variables until only one logical point of failure remains
5. **Trust facts, not intuition**: A beautiful theory means nothing without a stack trace to back it up

## Design Principles

6. **Don't overengineer**: Simple beats complex
7. **No fallbacks**: One correct path, no alternatives
8. **One way**: One way to do things, not many
9. **Clarity over compatibility**: Clear code beats backward compatibility
10. **Throw errors**: Fail fast when preconditions aren't met
11. **No backups**: Trust the primary mechanism
12. **Separation of concerns**: Each function should have a single responsibility

## Development Methodology

13. **Surgical changes only**: Make minimal, focused fixes
14. **Evidence-based debugging**: Add minimal, targeted logging
15. **Fix root causes**: Address the underlying issue, not just symptoms
16. **Simple > Complex**: Complexity for its own sake is a form of technical debt
17. **Collaborative process**: Work with user to identify most efficient solution

---

See [AGENTS.md](AGENTS.md) for what these mean in this repository, and
[docs/development.md](docs/development.md) for the layout and the build.

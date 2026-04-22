# TODO.md

## Small items

- [ ] if coordinator does not have redis configured, it should operate in a single node mode and should handle broadcasting itself. Update coordinator's readme with this information.

## Large items

- [ ] with small messages dominating broadcast, coordinator might hit a "Packets Per Second" wall. Check if this is true, and find ways to optimize it.
- [ ] coordinator should also support HTTP, processor should be able to choose through configs.

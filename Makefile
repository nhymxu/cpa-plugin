PLUGIN ?= opencode-free

.PHONY: help version bump tag release

help:
	@echo "make version [PLUGIN=opencode-free]              show current registry.json version"
	@echo "make bump PLUGIN=opencode-free VERSION=0.1.5      bump registry.json version"
	@echo "make tag PLUGIN=opencode-free VERSION=0.1.5       bump + create local git tag {plugin}-{version}"
	@echo "make release PLUGIN=opencode-free VERSION=0.1.5   bump + commit + tag + push (pushes to remote)"

version:
	@python3 -c "import json; print(next(p['version'] for p in json.load(open('registry.json'))['plugins'] if p['id'] == '$(PLUGIN)'))"

bump:
	@test -n "$(VERSION)" || { echo "VERSION is required, e.g. make bump PLUGIN=$(PLUGIN) VERSION=0.1.5" >&2; exit 1; }
	python3 scripts/bump_version.py $(PLUGIN) $(VERSION)

tag: bump
	git tag $(PLUGIN)-$(VERSION)
	@echo "Tag created locally. Push with: git push origin $(PLUGIN)-$(VERSION)"

release: bump
	git add registry.json
	git commit -m "chore($(PLUGIN)): bump version to $(VERSION)"
	git tag $(PLUGIN)-$(VERSION)
	git push origin main $(PLUGIN)-$(VERSION)

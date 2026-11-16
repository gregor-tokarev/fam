---
name: release-notes
description: Write release notes from the merged pull requests since the last tag. Use when the user asks for release notes or a changelog.
---

1. Find the last tag: `git describe --tags --abbrev=0`.
2. List the changes since that tag: `git log --first-parent --format='%h %s' <tag>..HEAD`. This shows merge commits and squashed pull requests, and each names its pull request (#123). Read a pull request with `gh pr view 123` when its title is not enough.
3. Group them as Added, Changed, and Fixed. Write one line for each change, for the users of the product, not for its developers.
4. Show the notes to the user. Do not publish them.

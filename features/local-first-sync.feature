Feature: Local-first folder sync
  As the owner of one folder living on several of my own devices,
  I want every change to propagate automatically while files are always
  written locally,
  so that my devices stay identical and an edit is never silently lost.

  # The persona "Sam" is the single owner-operator syncing his own vault.
  # v1 devices are desktop and laptop; the phone (Android) is deferred to v2.
  Background:
    Given Sam has enrolled his desktop and his laptop with the same key
    And both devices sync the folder as a local tree

  Rule: A local change propagates to every enrolled device

    Scenario: A new file appears on the other device
      When Sam adds "projects/tendrils.md" on his desktop
      Then "projects/tendrils.md" appears on his laptop with identical content

    Scenario: An edit propagates to the other device
      Given "notes/ideas.md" exists on both devices
      When Sam edits "notes/ideas.md" on his laptop
      Then his desktop copy updates to match

    Scenario: A rename propagates as a rename
      Given "draft.md" exists on both devices
      When Sam renames "draft.md" to "final.md" on his desktop
      Then his laptop shows "final.md" and no longer shows "draft.md"

  Rule: Incoming changes are written locally and atomically, never as a partial file

    Scenario: A large incoming file only appears once fully written
      When a 2 MB attachment arrives from Sam's desktop
      Then his laptop never exposes a truncated version of the attachment
      And any agent reading the folder sees only the complete file

    Scenario: A file being written is not published until it settles
      When Sam's editor is still writing "long-note.md" on his desktop
      Then Tendrils waits until the file stops changing before publishing it

  Rule: On divergence the newest write wins and the loser is preserved

    Scenario: The same file diverges while one device is offline
      Given Sam's laptop is offline
      And Sam edits "daily/2026-07-02.md" on his laptop
      And an agent rewrites "daily/2026-07-02.md" on his desktop later
      When Sam's laptop reconnects
      Then the newest version of "daily/2026-07-02.md" is present on both devices
      And the losing version is preserved as a conflict copy
      And no edit is destroyed

  Rule: A delete is absolute and always wins, but stays recoverable

    Scenario: A delete removes the file everywhere
      Given "old.md" exists on both devices
      When Sam deletes "old.md" on his laptop
      Then "old.md" is removed from his desktop

    Scenario: A delete beats a concurrent edit
      Given "stale.md" exists on both devices
      And Sam deletes "stale.md" on his laptop
      And an agent edits "stale.md" on his desktop before the delete arrives
      When the devices reconcile
      Then "stale.md" is removed from both devices
      And the edited version is kept in the trash for recovery

    Scenario: A deleted file can be recovered within the retention window
      Given Sam deleted "receipt.md" two days ago
      When Sam looks in the trash
      Then "receipt.md" is still recoverable

    Scenario: The trash is pruned after the retention window
      Given a file has been in the trash for more than seven days
      When housekeeping runs
      Then the file is purged from the trash

    Scenario: Re-creating a deleted filename later is honoured
      Given Sam deleted "notes.md" last week
      When Sam creates a brand-new "notes.md" on his desktop today
      Then "notes.md" syncs normally to his laptop

  Rule: File contents are encrypted before they leave the device

    @question
    # Open: exact symmetric-key derivation from the Nostr key. See OPEN-QUESTIONS.md (Q8).
    Scenario: A stored blob is unreadable without Sam's key
      When a file is uploaded to the Blossom server
      Then anyone who fetches the blob by its hash receives only ciphertext
      And only a device holding Sam's key can decrypt it

  Rule: Devices need not be online at the same time

    Scenario: A change made while the other device is off is applied later
      Given Sam's laptop is powered off for a week
      And Sam changes several files on his desktop during that week
      When Sam's laptop is powered on
      Then it reconciles and receives every change it missed

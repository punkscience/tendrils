Feature: Status and observability
  As someone who builds this tool to trust and repair it,
  I want to see at any moment what Tendrils is doing and what it has done,
  so that when sync misbehaves I can understand and fix it.

  Rule: The tray icon reflects live sync state at a glance

    Scenario: The tray shows an idle state when caught up
      Given Sam's device is fully in sync
      Then the tray icon shows the idle state

    Scenario: The tray shows a working state while syncing
      When Tendrils is uploading or pulling files
      Then the tray icon shows the syncing state

    Scenario: The tray shows an error state on failure
      Given the relay is unreachable
      Then the tray icon shows the error state

  Rule: Current status is reported on demand

    Scenario: The owner inspects the current status
      When Sam asks Tendrils for its status
      Then it reports the sync root path
      And it reports the time of the last successful reconcile
      And it reports how many files are pending upload
      And it reports any current conflicts
      And it reports whether the relay and storage are reachable

  Rule: An unreachable relay or server is retried, then escalated

    Scenario: A brief outage recovers without owner involvement
      Given the relay becomes unreachable
      When it comes back within a few minutes
      Then Tendrils resumes syncing on the next retry
      And no changes were lost while it was unreachable

    Scenario: A prolonged outage is escalated to a critical issue
      Given the relay has been unreachable for twelve hours
      Then Tendrils reports it as a critical issue
      And it stops retrying until the owner intervenes

  Rule: Activity is logged legibly enough to diagnose a problem

    Scenario: A successful sync is recorded with what and when
      When "daily/2026-07-02.md" is pushed to the set
      Then the log records the file, its content hash, and the time

    Scenario: A failure is recorded with its cause
      Given an upload fails because the storage server is unreachable
      Then the log records the failure and names the cause

Feature: Device setup and enrollment
  As someone who hates passwords and owns several devices,
  I want to enrol a device with a single key and point it at a local folder,
  so that the device joins my sync set with no accounts and no per-device pairing.

  Rule: Enrollment requires only the Nostr key

    Scenario: A new device is enrolled with the key alone
      Given Sam has already synced his desktop
      When Sam enrolls his laptop with the same key
      Then the laptop joins his sync set
      And no password or account is required

  Rule: The owner chooses the local folder that becomes the sync root

    Scenario: The owner selects the sync root during setup
      When Sam sets up Tendrils on his laptop
      Then Tendrils prompts him to choose a local folder
      And the chosen folder becomes the sync root on that device

    Scenario: Each device may place the sync root wherever it likes
      Given Sam's desktop syncs the folder at "C:\Vault"
      When Sam chooses "/home/sam/vault" as the sync root on his laptop
      Then both devices stay in sync despite the different paths

  Rule: A freshly enrolled empty folder is filled by the initial reconcile

    Scenario: An empty folder receives the current tree
      Given Sam enrols a device and selects an empty folder
      When the initial reconcile completes
      Then the folder holds the current version of every synced file

  Rule: Enrolling a non-empty folder union-merges it with the sync set

    Scenario: Existing local files are published to the sync set
      Given Sam's laptop already contains a copy of his vault
      When Sam enrols the laptop and selects that existing folder
      Then files it has that the set lacks are published to the set
      And files the set has that it lacks are pulled down

    Scenario: Drifted copies produce conflict copies, never loss
      Given Sam's existing laptop vault differs from the set for "index.md"
      When Sam enrols the laptop on that folder
      Then the newest "index.md" becomes the shared version
      And the other version is preserved as a conflict copy

  Rule: The key is stored securely so it need not be re-entered

    Scenario: The daemon resumes after a reboot without re-entering the key
      Given Sam has enrolled his laptop
      When Sam reboots the laptop
      Then Tendrils resumes syncing without asking for the key again

  Rule: The key carries the relay and storage configuration

    Scenario: A device discovers where to sync from the key
      When Sam enrols a device with only his key
      Then the device discovers his relays and Blossom servers published under that key

    Scenario: A local config overrides the published configuration
      Given Sam has a local config naming his own relay
      When the device starts
      Then it uses the relay from the local config instead of the published one

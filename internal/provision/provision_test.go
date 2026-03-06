package provision

// Tests for provision package are split into 5 focused test files:
// - provision_validation_test.go: IsValidAgentID, IsValidBotToken, IsValidUserID, ResolveModelAlias
// - provision_config_test.go: GenerateAgentBlock, GenerateCrontab, template rendering
// - provision_files_test.go: File operations (copyDir, copyFile, templateSoulFile)
// - provision_integration_test.go: Full Provision workflows (defaults, openclaw, blank, copy modes)
// - provision_util_test.go: Utility functions (StaggerCrontabLine, TitleCase, ToSlug, SeedDefaults)

// The original monolithic test file contained 1014 lines covering these domains.
// Tests have been distributed across 5 new focused files by domain.

// Verify no more tests are needed in this file beyond documentation.
// This module acts as documentation pointing to the split test files.

// --- Documentation of test organization ---

// Validation tests (provision_validation_test.go):
// - TestIsValidAgentID: Validates agent ID rules (lowercase, hyphens only)
// - TestIsValidBotToken: Validates Telegram bot token format
// - TestIsValidUserID: Validates numeric user ID constraints
// - TestResolveModelAlias: Tests opus/sonnet/haiku alias resolution

// Configuration generation tests (provision_config_test.go):
// - TestGenerateAgentBlock: Basic TOML agent block generation
// - TestGenerateAgentBlockCustomSystemFiles: Custom system_files array
// - TestGenerateCrontabFromTemplate: Crontab template processing
// - TestGenerateCrontabStagger: Staggered minute times for multiple agents
// - TestGenerateCrontabMissing: Error handling for missing templates

// File operation tests (provision_files_test.go):
// - TestTemplateSoulFile: Placeholder substitution in SOUL.md
// - TestCopyDir: Directory file copying
// - TestCopyFile: Individual file copying with error cases
// - Edge cases: Permission errors, missing files, unreadable directories

// Integration tests (provision_integration_test.go):
// - TestProvisionDefaults: Full defaults mode provisioning
// - TestProvisionOpenclaw: Openclaw character mode
// - TestProvisionBlank: Empty character files in blank mode
// - TestProvisionCopy: Copying from existing agent workspace
// - TestSeedDefaults: Seeding defaults directory without overwriting

// Utility function tests (provision_util_test.go):
// - TestStaggerCrontabLine: Minute staggering with wrapping
// - TestTitleCase: Hyphen-separated to title case conversion
// - TestToSlug: Display name normalization
// - TestAppendCrontab: Crontab command execution

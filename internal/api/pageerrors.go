package api

// A foreign cursor can fail different decoders on the same endpoint. These
// messages come from the source/target pairs in the 2026-10-31 cursor-parent
// binding recording. Pairs with the same message as the target's existing
// fallback need no entry.
var foreignCursorWords = map[cursorList]map[cursorList]string{
	cursorAgents: {
		cursorEnvironments:     "invalid page cursor",
		cursorMemoryStores:     "unsupported page token version 2",
		cursorVaults:           "invalid page cursor",
		cursorSkills:           "invalid page cursor",
		cursorVaultCredentials: "invalid page cursor",
		cursorAgentVersions:    pageTokenOtherEndpoint,
		cursorSkillVersions:    "invalid page cursor",
		cursorMemories:         "unsupported page token version 10",
		cursorMemoryVersions:   "invalid page cursor",
		cursorSessionEvents:    pageTokenOtherEndpoint,
	},
	cursorEnvironments: {
		cursorAgents:       "Invalid page token: invalid base64 encoding",
		cursorSessions:     "Invalid page token: invalid base64 encoding",
		cursorMemoryStores: "Invalid page token: invalid base64 encoding",
		cursorVaults:       "Invalid page token: invalid base64 encoding",
		cursorSkills:       "Invalid page token format: expected 'env_id|version'",
	},
	cursorSessions: {
		cursorEnvironments: "invalid page cursor",
		cursorMemoryStores: "invalid page cursor",
		cursorVaults:       "invalid page cursor",
		cursorSkills:       "invalid page cursor",
	},
	cursorMemoryStores: {
		cursorEnvironments: pageTokenInvalid,
		cursorVaults:       pageTokenInvalid,
		cursorSkills:       pageTokenInvalid,
	},
	cursorVaults: {
		cursorAgents:       "page: invalid cursor",
		cursorEnvironments: "page: invalid cursor",
		cursorSessions:     "page: invalid cursor",
		cursorMemoryStores: "page: invalid cursor",
		cursorSkills:       "page: invalid cursor",
	},
	cursorVaultCredentials: {
		cursorAgents: "page: invalid cursor",
	},
	cursorAgentVersions: {
		cursorAgents: pageTokenOtherEndpoint,
	},
	cursorSkillVersions: {
		cursorAgents: "page is not a valid page token.",
	},
	cursorMemoryVersions: {
		cursorAgents: pageTokenInvalid,
	},
}

func foreignCursorError(target cursorList, c *cursor, fallback string) error {
	if words := foreignCursorWords[target][c.list]; words != "" {
		return errInvalid("%s", words)
	}
	return errInvalid("%s", fallback)
}

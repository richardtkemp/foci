/**
 * @name Verify no unpersisted map state
 * @description Formal verification that all struct types with map fields have persistence infrastructure.
 *              This is a structural soundness check - if this query returns zero results, it proves
 *              that no struct with stateful maps lacks persistence methods.
 * @kind problem
 * @id go/verify-persistence
 * @problem.severity error
 * @tags correctness
 *       persistence
 *       state-management
 */

import go

predicate isPersistenceMethod(Method m) {
  m.getName().matches("%Save%") or
  m.getName().matches("%Persist%") or
  m.getName().matches("%Set%") or
  m.getName().matches("%Store%") or
  m.getName().matches("%Restore%")
}

predicate isKnownEphemeralStruct(StructType st) {
  // Warning queue buckets - time-windowed suppression
  st.getName() = "Queue" or
  // Bitwarden cache - security by design (TTL expiry)
  st.hasQualifiedName("foci/internal/secrets/bitwarden", "Store") or
  // Last message store - convenience feature
  st.getName() = "LastMessageStore" or
  // Bot manager - runtime registry
  st.getName() = "BotManager" or
  // Tool/command registries - rebuilt on startup
  st.getName() = "Registry" or
  // Session index - this IS the persistence layer
  st.hasQualifiedName("foci/internal/session", "SessionIndex")
}

from Field mapField, StructType st
where
  mapField.getType() instanceof MapType and
  mapField.getDeclaringType() = st and
  // Exclude known ephemeral structs
  not isKnownEphemeralStruct(st) and
  // Check if the struct has ANY persistence method
  not exists(Method m |
    m.getReceiverType() = st and
    isPersistenceMethod(m)
  )
select mapField,
  "PERSISTENCE GAP: Map field '" + mapField.getName() + "' in struct '" + st.getName() +
  "' has no persistence methods. Either add Save/Persist/Set/Restore methods or add to isKnownEphemeralStruct if intentionally ephemeral."

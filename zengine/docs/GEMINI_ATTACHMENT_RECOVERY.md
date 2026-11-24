# Gemini attachment pruning failure

The `Notes_Agent/sessions/test` log was inspected read-only on October 15, 2026.
No provider requests were made and no session events or blobs were changed.

## Cause

Event 618 pruned one attachment because a 31 MB request exceeded the configured
28 MB limit. Event 619 then reported a 400 at
`contents[6].parts[0].function_response.parts[0].text`.

The old Gemini serializer treated nested function-response parts like ordinary
content parts. After pruning, the image payload became a text placeholder in
that nested array. Gemini only permits `inlineData` there; explanatory text
belongs in the structured `functionResponse.response` object.

The portable retry pruned every remaining attachment, creating the same invalid
text placeholder at ten locations. Continuing the session reconstructed the
same invalid request. This was a serialization error, not loss of tool results
or failed image generation.

The checkout already contained the serializer correction dated October 14.
The installed `Notes_Agent/notes_agent.exe` was dated October 13. A running
process must be restarted with a rebuilt executable to use corrected source.

## Additional protection

`internal/provider/gemini_validate.go` preflights the final request, including
`extra_body` overrides, before request-size measurement. It relocates misplaced
nested text to `response.attachment_notes`, retaining existing notes, output or
errors, call IDs, media and thought signatures. Valid request bytes stay
unchanged. Ambiguous nested parts are rejected locally with their exact path;
the guard does not guess at unknown schemas or discard tool results.

This complements the existing context repair for missing and orphaned tool
results. It is not a universal validator for all provider schemas or errors.

## Verification

The saved session reconstructs into 169 messages and 76 function responses.
Normal mode has a removed tool attachment at contents index 6. Portable mode
has removed tool attachments at indices 6, 16, 30, 46, 66, 135, 143, 151, 161,
167: exactly the ten indices in the reported provider error. Normal, portable
and summary requests pass offline replay with no nested text parts.

Provider and engine tests pass outside the Windows sandbox. The full suite has
a separate reproducible failure in `TestCancelKillsForegroundCommand` in
`internal/tools`; that cancellation behavior was not changed by this fix.

The rebuilt Windows binary is `dist/notes_agent-fixed.exe`, version
`attachment-recovery-dev`. The installed executable and running service have
not been replaced. After `/quit` stops the old service, resume the existing
session using PowerShell:

```powershell
$agentHome = 'C:\Users\ranjan\Music\Notes_Agent'
& 'C:\Users\ranjan\Music\zagent\dist\notes_agent-fixed.exe' --home $agentHome --workspace $agentHome --session test --ui web
```

Send `continue` after the new service opens. No deletion, rewriting or
compaction of the saved session is needed for this serialization fix. Live
provider acceptance has not been tested.

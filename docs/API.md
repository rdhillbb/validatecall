# checknumber API

For the system that calls checknumber after a fax fails. Written 2026-11-14 from the code in
`internal/httpapi/` and the approved product description.

checknumber dials one phone number through Telnyx and says what answered. A number that was
checked in the last 60 days is not dialled again: the saved result comes back at once.

## Before you integrate

- **Where it answers:** `http://127.0.0.1:<port>`. The port comes from the port registry
  (name `checknumber`; 5123 on the operator's Mac today). It listens on the local machine only.
  A caller on another machine cannot reach it yet; where the calling system runs is still an
  open decision.
- **Key:** when the setting `CHECKNUMBER_API_KEY` is set on the server, every `/api/` request
  must carry `Authorization: Bearer <key>` (the word `Bearer` exactly, one space). Without it
  the reply is `401`. When the setting is not set, no key is needed.
- **One number per request.** Calls are placed one at a time; requests for other numbers wait
  their turn.
- **Set your HTTP timeout to at least 180 seconds.** A new call takes up to about 2 minutes,
  plus the wait behind a call already in progress. A saved result returns in milliseconds.
- **New with the build of 2026-11-14:** the result `international`.
- **Two values change with the build of 2026-11-14** (series `checknumber-r2`): the code
  `answered_no_fax_tone` becomes `picked_up`, and the reuse window becomes 60 days (was 30).
  This document describes the API after that build.

## POST /api/check

Checks one number and waits for the result.

Request:

```http
POST /api/check
Content-Type: application/json
Authorization: Bearer <key>        (only when the server has a key set)

{"number": "+1 212 555 0142"}
```

- `number`: a US or Canada number, or a number of another country written with `+` and its country code (that one is not dialled, see `international`). Accepted US and Canada forms: `+12125550142`, `12125550142`,
  `2125550142`, with spaces, dashes, dots or brackets. No extensions.
- The body must be exactly one JSON object. Anything after it is refused.

Reply `200`:

```json
{
  "number": "+12125550142",
  "result_code": "voice_message",
  "result_label": "Voice message",
  "called_at": "2026-11-14T04:46:21Z",
  "saved": false,
  "reason": "",
  "text": "Our office is currently closed. Please try your call again during our normal business hours."
}
```

| Field | Type | Meaning |
|---|---|---|
| `number` | string | The number in the form `+1NXXNXXXXXX` |
| `result_code` | string | One of the codes below. Use this field in code |
| `result_label` | string | The same result in words, for showing to a person |
| `called_at` | string | When the call was placed, UTC, RFC 3339. For a saved result this is the time of the earlier call |
| `saved` | boolean | `true`: no call was placed now; this is the saved result of the call at `called_at`. `false`: a call was just placed |
| `reason` | string | Filled when `result_code` is `unknown` (why the app could not tell) or `international` (not called). Otherwise empty |
| `text` | string | Filled only for `voice_message`: the first seconds of the recorded greeting as text, or `not available: <why>`. Otherwise empty |

## Result codes

| `result_code` | `result_label` | Meaning |
|---|---|---|
| `fax` | Fax | The line answered and a fax machine was detected |
| `voice_message` | Voice message | The line answered with a recorded greeting; see `text` |
| `person_answered` | Person answered | A live person answered. The app hung up and recorded nothing |
| `picked_up` | Picked up | The line answered, but the app could not tell what answered |
| `no_answer` | No answer | The line rang about 5 times (30 seconds) and nobody picked up |
| `busy` | Busy | The line gave a busy signal |
| `unallocated` | Unallocated | The phone network says the number does not exist |
| `disconnected` | Disconnected | The phone network rejected the call |
| `disconnected_unallocated` | Disconnected / Unallocated | Used in place of the two above when the server is set to combine them |
| `international` | International | A number of a country other than the US and Canada (it starts with `+` and a country code other than 1). It is saved and never dialled; `reason` is `not called: outside US and Canada`; `called_at` is the time the request was recorded |
| `unknown` | Unknown | The call failed or ended in a way the app could not classify; see `reason`. Follow up by hand |

Known limit, seen on real calls on 2026-11-14: Telnyx did not report a fax on five working fax
lines. A working fax machine therefore usually comes back as `picked_up`, not `fax`. Treat
`picked_up` as "the line is alive and something answers".

## When a number is dialled again

- Any result except `unknown` is kept for 60 days. A request inside that time returns the saved
  result with `saved: true` and places no call.
- After 60 days the next request dials again. Earlier results stay in the history.
- A number whose last result is `unknown` is dialled again on every request.
- A second request for a number that is being called right now waits and gets that same result.

## GET /api/history?number=...

Every check of one number, newest first.

```http
GET /api/history?number=%2B12125550142
```

Reply `200`:

```json
{"checks": [ { "number": "+12125550142", "result_code": "busy", "result_label": "Busy",
               "called_at": "2026-11-14T04:33:22Z", "saved": true, "reason": "", "text": "" } ]}
```

Each entry has the same fields as the check reply. An empty list means the number was never
checked.

## Errors

Every error reply is JSON: `{"error": "<message>"}`.

| Status | When | `error` |
|---|---|---|
| `400` | The body is not one JSON object with `number` | `request body must be JSON {"number": "..."}` |
| `400` | Empty number | `Enter a phone number.` |
| `400` | Not a valid +1 number | `This is not a valid US or Canada number.` |
| `401` | Key missing or wrong (only when the server has a key set) | `missing or wrong key` |
| `500` | Anything else | `internal error` |

A refused number (`400`) is neither saved nor dialled. A wrong method or path gets a plain-text
`405` or `404`, not JSON.

## Example

```bash
curl -s -m 180 -X POST "http://127.0.0.1:$CHECKNUMBER_PORT/api/check" \
  -H "Content-Type: application/json" \
  -d '{"number":"+1 212 555 0142"}'
```

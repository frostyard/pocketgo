# 0029 — Attach the owner's files to a message, stored outside the data folder

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner cannot give an agent a file.** A message is text alone, so a screenshot, a log or a document reaches an agent only through a path the owner types, on a host the owner may not have the file on.

**frostyard/pocket and pi-pocket have attachments.** pocket's composer has an "Attach files" button with a native file chooser, takes a pasted image and files dropped on it, and shows each file as a chip with its name, "uploading…" or "upload failed", and a Remove button. A file is at most 50 MB, and Send waits until every upload is done. pi-pocket's server stores each upload under a name of its own, names every file in the message by its path, name, type and size, so the agent can open it with its tools, and gives an image to a model that takes images as an image, made small enough first: at most 5 MB as a file, at most 2000 pixels a side as the model gets it.

**The owner decided on 2026-11-20:**
- Port pocket's whole: the button, the pasted image (a PNG on the clipboard), the dropped files, the chips, 50 MB a file, Send waiting for the uploads, images as images when the model takes them, and every file named by its path on the service host with its name, type and size.
- Uploads live in `~/.local/share/pocketgo-uploads/<session>/` of the service user. That is outside pocketd's data folder, which the agents' sandbox hides ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)), so the agents can read the files in the sandbox. Uploads never go inside a repository, and stay until the owner removes them by hand.
- A session with no composer, a worker's or a cross-review's, takes no attachment, as it takes no message.

**Agents can write where uploads live.** Agents run as pocketd's own Unix user. In the sandbox they can change everything but pocketd's data folder and start files, so they can write in the uploads folder and plant a symbolic link there. pocketd writes and reads that folder outside the sandbox, with its own rights. A link it followed could lead a write into its data folder, or a read out of it, and send a sign-in to a model.

**PiG takes images in a user message.** A submission's content may be `ai.UserContentBlocks` of text and `ai.ImageContent` blocks. A model's catalog entry lists `image` among its inputs when it takes images. PiG's own image resizer is internal to it.

**The protocol has a shape for requests.** Every `POST` carries a `requestId` in a JSON body of at most 1 MiB (rules 3 and 4), and a retry with it does nothing twice (rule 5).

## Decision

1. **An upload is its own request** (protocol version 22, rule 10a). `POST /v1/sessions/{id}/uploads?name=<name>&requestId=<id>` carries the file's bytes as its body, at most 50 MiB (`proto.MaxUpload`; more is 413), and its type as `Content-Type`.
   - The body is the file, so the `requestId` rides in the query string. A retry answers the upload the first call stored, and stores no other file.
   - pocketd answers `proto.Upload`: the upload's id, its safe name, its type, its size and its path on the service host. A new upload is 201, a retry 200.
   - A worker's or a cross-review's session is `conflict`, as a message to it is.
2. **pocketd stores each file in the uploads folder** (`service/internal/uploads`). The file goes in `~/.local/share/pocketgo-uploads/<session>/`, named `<stamp>-<random>-<name>` after pi-pocket, at mode 0600 in folders of mode 0700, with pi-pocket's safe name. pocketd claims it in the work database (migration 18, `uploads`) with its request before the file is written, keeps what tells the file apart once it is whole, and stores it once the file has its name. An upload cut off, too large or not stored before its name leaves no file and no record; one a stop of pocketd cut off after its name is finished when its request comes again or at the next start, while its name holds the file pocketd wrote. pocketd removes nothing in the uploads folder.
3. **pocketd follows no link an agent planted, and never waits on what one put there.** Each folder below `~/.local/share` opens by its descriptor, as a folder, not through a link and without waiting on a named pipe. A new file is written with no name (`O_TMPFILE`), else in a staging folder in pocketd's data folder, which no agent reaches, and named once whole, where no entry is; its name, and each folder it is in, are on disk before the upload counts, and a file named whose name did not reach the disk keeps its claim. A file counts only at the upload's path: one whose session's folder an agent moved as it was named is stored anew when its request comes again. An upload that could never be named, as where the file system of `~/.local/share` makes no unnamed file and the data folder is on another, is refused before its body is read. At its start pocketd clears what another run left in the staging folder: only regular files named as pocketd stages one, through the folder's descriptor, never through a link. A file is read only inside its session's folder, as a regular file, without waiting on a named pipe, and at most the bytes it was uploaded with.
4. **A message names its attachments by their upload ids** (`SendMessage.attachments`). Each must be an upload pocketd stored for that session, never a path a client names. With an attachment, the text may be empty. pocketd ends the message with the line `Attached files (saved on the server):` and one line a file, `- <path> (<name>, <type>, <size> bytes)`, from the upload's record; `proto` writes and reads that form, so the desktop shows what pocketd wrote. pocketd sends the heading and the lines as blocks of their own, apart from the owner's words, and counts the files from those blocks (`Entry.files`), so a list the owner wrote or pasted stays words.
5. **An image goes to the model as an image when the model that reads it takes images.** That means a PNG, JPEG, GIF or WebP file of at most 5 MiB, its format read from its bytes. Every message carries its images; the model that reads it, when it runs, gets them as images when it lists `image` among its inputs (`Model.images`), and any other model gets PiG's placeholder in their place, at each of its requests. A model change before the message runs, or after, so gives no model content it cannot take.
   - pocketd makes it fit in Go, as pi-coding-agent does: at most 2000 pixels a side and 4.5 MiB of base64, as PNG or JPEG, lowering the JPEG quality, then the size by a quarter. golang.org/x/image decodes WebP and resamples, halving first and then bilinear, so one image never takes much more memory than its own pixels.
   - A GIF goes as its first frame, on its canvas, made PNG or JPEG: its other frames are never decoded, so an animation takes no more memory than one frame.
   - An image that is not opaque goes as PNG alone: JPEG would drop its alpha.
   - The file's line notes what changed: the format, and the size with the factor that maps coordinates back.
   - An image that cannot be read, decoded or made to fit is named without an image, with a note why. So is one of more than 50 million pixels, which pocketd does not decode, and one past the message's 24 MiB of images, which stay in memory until it goes.
   - A user entry carries the number of images it went with (`Entry.images`).
6. **The desktop ports pocket's attachments whole.**
   - In the composer: Attach files, which opens the native chooser; a paste of files or of a PNG; files dropped on the field.
   - Each file shows as a chip with its name, "uploading…" or "upload failed", and Remove; a failed one offers Try Again, with its request id. Send waits until every chip has uploaded.
   - In the transcript, the owner's message shows its words, a chip for each file with its path in the tooltip, and the count of its images, as pocket shows them.
   - Its rules live in `desktop/internal/state`: what may be sent, a file's type, a source's size, the upload's request id. At most four files hold a file open at once, being copied or uploaded, or kept for Try Again after their upload failed; the others wait their turn. A Try Again of a kept copy goes ahead, and a removal takes its chip out at once and frees its slot once the copy or upload under way ends.
   - The desktop copies each file as it is attached, to its end and at most 50 MiB, and every try uploads that copy. A copy has no name on disk from the moment it is made (on Windows, where the file system takes POSIX deletion, and no other handle may open it), so a crash leaves nothing of it; it goes once its upload is done, its chip is removed, the owner signs out or the window closes, as its last reader ends. A pasted image's bytes go once its copy is made, and pasted bytes over the limit fail at once, kept by nothing. `proto.Client.Upload` sends the file again on a redirect only to the service's own origin (`proto.SameOrigin`, the desktop's rule, which compares hosts as the resolver looks them up, a final dot kept), with the same method and its token.
7. **The sandbox shows the uploads folder.** It lies outside every folder the sandbox hides. A test reads an upload with a command in `bwrap`, and pocketd warns at its start when a hidden folder holds the uploads folder.

## Consequences

- The owner gives an agent a file the way pocket did. The agent reads it with `read` or a command, at the path the message names, from any session but a worker's or a review's.
- Uploads pile up in the service user's `~/.local/share/pocketgo-uploads/` until the owner removes them. pocketd never cleans them, and they are not in the data folder's backup.
- Agents can change or remove an upload. The message names the file as it was uploaded; an image the model got with the message is what pocketd read when it went.
- A message with images is as large in PiG's store as its images, up to 4.5 MiB of base64 each. pocketd sets no limit on how many a message carries.
- HEIC photos are named, but not shown to the model: Go has no decoder for them. An image pocketd resizes loses its EXIF orientation.
- Protocol version 22 (versions 18, 20 and 21 never shipped with attachments: the schedules of ADR-0030 took 19, `team_send` of ADR-0032 took 20, and the gathers of ADR-0033 took 21), work database migration 18, and a new dependency of the service, `golang.org/x/image`. The desktop must be rebuilt.

## Alternatives considered

- **The file in the JSON body, as base64:** a JSON body stays below 1 MiB, and base64 adds a third. pocket sends the raw bytes.
- **Multipart form data:** one file a request needs no parts, and pocket sends one.
- **Uploads in the data folder, as pi-pocket keeps them:** the sandbox hides the data folder from the agents, who could not open the files.
- **Uploads in the session's working folder:** it is often a repository's checkout, and the owner wants uploads never inside a repository.
- **A path the client names:** pocketd would read whatever it is told to, its own sign-ins included.
- **An uploads folder read-only to the agents:** not asked for; it is left as an open question to the owner.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 1, 3, 4, 5, 7a, 10, 10a and 16a), [design/architecture.md](../design/architecture.md)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 14, attachments)
- Builds on: [ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md) (a standing session's profile and Start Over change its model), [ADR-0028](0028-let-the-owner-change-a-sessions-model-from-the-composer.md) (the owner's change of a session's model)

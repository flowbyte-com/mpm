// Session C — minimal project-identity probe.
// Records ctx.location only. Used to test whether the host's project id is
// stable across linked worktrees of one repository.
import { appendFileSync } from "node:fs"

const OUT = process.env.PROBE_OUT || "/tmp/opencode/probe-c/out/loc.jsonl"

export default {
  id: "session-c-loc-probe",
  async setup(ctx) {
    try {
      appendFileSync(
        OUT,
        JSON.stringify({
          t: Date.now(),
          pid: process.pid,
          cwd: process.cwd(),
          location: {
            directory: ctx.location?.directory,
            workspaceID: ctx.location?.workspaceID,
            project: ctx.location?.project
              ? {
                  id: ctx.location.project.id,
                  directory: ctx.location.project.directory,
                  canonical: ctx.location.project.canonical,
                }
              : null,
          },
        }) + "\n",
      )
    } catch {
      /* probe must never break the host */
    }
    return () => {}
  },
}

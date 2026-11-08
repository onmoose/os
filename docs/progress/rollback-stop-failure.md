# A rollback does not touch data while the app may still run

- **Status:** done
- **Date:** 2026-11-08
- **Specs touched:** `UPDATES.md` (# As built (#631)), `APP_LIFECYCLE.md` (# update + rollback).

Greptile found this on the release PR #650. It is a fix to the rollback from #631 (`UPDATES.md` # As built (#631)) and the rollback by hand from #634.

`Manager.undoUpdate` ran `compose stop` and only logged a warning when it failed. It then moved the managed databases back and restored the snapshot, which clears the app's data folders and rebuilds its databases. So after a failed stop, data could be replaced while the app's containers still wrote to it. A failed update, the rollback by hand and the startup pass all run this code, and with the automatic update on by default it can run every night on every box.

Returning the stop's error was not the answer. `compose stop` reads `compose.yml`, `compose.override.yml` and `.env` on disk. During a rollback these can be the new version's files, and they can be why the update failed. The stop can then fail although the containers stopped long ago, and every rollback of that app would fail in the same way, for good.

## What was done

- **`stopForUndo` in `internal/lifecycle/update.go`** runs before the undo changes anything, whenever the undo used to run the stop. `compose stop` working is enough. When it fails, the brain asks Docker which containers of the project `moose-<id>` run, and goes on when there are none. When some run, it stops them by id, and asks again. It goes on only if none are left.
- **It fails closed.** Containers that still run, or a Docker that does not answer, return an error before `moveServicesBack`, so nothing is changed. A direct stop that returns an error does not decide anything, because the container may have gone already: the second question decides.
- **Two driver methods,** `RunningContainers` (`docker ps -q --filter label=com.docker.compose.project=<project>`) and `StopContainer` (`docker stop <id>`). No method already there could do this: `Inspect` looks at one service by `moose.instance_id`, and `ManagedContainers` lists every app and reads a label the override sets. They read no compose file. `docker stop` uses the stop timeout the container was created with, so an app's `stop_grace_period` still counts.
- **The socket proxy needs no change.** `docker ps` is `GET /containers/json`, which `Inspect` already uses, and `docker stop` is `POST /containers/<id>/stop`, the same call `compose stop` makes. Both are in the `CONTAINERS` + `POST` families of `proxyAllowlist`.
- **What each caller does was checked.** All three keep `update.json` and leave the row `update_failed`, and Start then runs the undo again.
  - A failed update says "moose could not put the old version back", and the app's page says "moose could not put the app's data back". Both are true.
  - The rollback by hand says "moose could not go back to version X. Start the app to try again". This is true.
  - The startup pass says "The update did not finish, and moose could not put the old version back" for an update, and records no failure for a rollback by hand. This is true.
  - **The Start retry was the one message that needed a change.** It said "See the app's logs", but the reason is in the brain's log and not in the app's. It now says "moose could not stop the app, so it cannot put the old version back yet. Try again". This is the only reason for the `errAppNotStopped` sentinel.
- **Tests, in `internal/lifecycle/rollback_stop_test.go`.** Each case runs on the failed update and on the rollback by hand, on an app whose database the update moved.
  - The rollback goes on: nothing runs, the direct stop works, and a direct stop fails on a container that has gone. It ends on the old version with the old data, and the check and the direct stops come before any database change.
  - The rollback does not start: the containers keep running (the stop returns an error, or returns nil and they still run), Docker cannot answer, and Docker stops answering after the direct stop. The data, the files, the grant and the record stay. A Start while it is still stuck says why and changes nothing. A later Start finishes the rollback with the compose files still broken.
  - The startup pass: an interrupted update with nothing running, an interrupted update whose containers keep running, and an interrupted rollback by hand when Docker cannot answer.
  - I broke the fix on purpose in three ways: the old behaviour, returning the stop's error, and no second question. The tests failed each time.
- **Against real Docker:** `TestLiveRollbackBrokenComposeFiles` (`dockerlive` tag) updates a real app, writes a broken `compose.yml` while it runs, and rolls back. `compose stop` fails, the container is found by its label and stopped by id, and 1.0 runs again on 1.0's data. It passed on a local daemon.

## Known gaps

- The live test talks to Docker directly, not through the socket proxy. The proxy rule was read, not run, for these two calls. `compose stop` and `docker ps` already use the same two endpoints through the proxy on a real box.
- A `compose stop` that succeeds is trusted, as before. It stops only the services the files on disk name, so a container from a service those files do not list would not be stopped.
- While the row is `update_failed` after a stop that did not work, the app's containers may still run. The route shows the "failed" splash, so nobody reaches the app, but what runs inside can go on writing until a Start stops it.

## What's next

Nothing planned.

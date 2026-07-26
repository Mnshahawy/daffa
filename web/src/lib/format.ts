// Daffa's domain verb maps. The generic time/byte/sha formatters moved to the shared
// kit; re-exported so every existing `@/lib/format` import keeps working.
export {
  ago,
  absolute,
  duration,
  elapsed,
  humanMs,
  shortSha,
} from '@mnshahawy/daffa-console-ui'

// The action names are compose's, not English. `up` is a fine thing to type at a shell and a
// poor thing to read in a sentence — "The up failed" is not something a person would say. So the
// wire keeps the verb and the UI says the words.

/** For a button or a heading: "Deploy". */
export function actionLabel(action: string): string {
  return (
    {
      up: 'Deploy',
      pull: 'Pull + deploy',
      restart: 'Restart',
      stop: 'Stop',
      down: 'Down',
      'down+volumes': 'Down + volumes',
    }[action] ?? action
  )
}

/** For the middle of a sentence: "The deploy failed." */
export function actionNoun(action: string): string {
  return (
    {
      up: 'deploy',
      pull: 'pull and deploy',
      restart: 'restart',
      stop: 'stop',
      down: 'teardown',
      'down+volumes': 'teardown',
    }[action] ?? action
  )
}

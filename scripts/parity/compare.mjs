// Compare the case results from the real table and the emulator, printing a diff for each case that differs.
// Exits 1 on any difference.
// Usage: node compare.mjs <real.json> <emulator.json>
import assert from 'node:assert'
import { readFileSync } from 'node:fs'

// Timestamps above this are the server's clock, as in ReadModifyWriteRow cells. The cases write timestamps far below it.
const SERVER_CLOCK_MICROS = 1e15

const blankServerTimestamps = (key, value) => ((key === 'ts' || key === 'timestamp') && Number(value) > SERVER_CLOCK_MICROS ? '<now>' : value)
const [real, emulator] = process.argv.slice(2).map((path) => JSON.parse(readFileSync(path, 'utf8'), blankServerTimestamps))

const names = [...new Set([...Object.keys(real), ...Object.keys(emulator)])]
let differing = 0
for (const name of names) {
    try {
        assert.deepStrictEqual(emulator[name] ?? null, real[name] ?? null)
    } catch (err) {
        differing++
        console.log(`--- ${name} (+ emulator, - real)\n${err.message.split('\n').slice(3).join('\n')}`)
    }
}
console.log(`${names.length - differing} of ${names.length} cases match.`)
process.exit(differing ? 1 : 0)

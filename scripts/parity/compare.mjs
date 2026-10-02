// Compare the case results from the real table and the emulator, printing a diff for each case that differs.
// Exits 1 on any difference.
// Usage: node compare.mjs <real.json> <emulator.json>
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// Timestamps above this are the server's clock, as in ReadModifyWriteRow cells. The cases write timestamps far below it.
const SERVER_CLOCK_MICROS = 1e15

// Sort object keys, since the two targets return families in different orders, and blank server-chosen timestamps.
const normalise = (value, key) => {
    if (Array.isArray(value)) return value.map((v) => normalise(v))
    if (value && typeof value === 'object') {
        return Object.fromEntries(
            Object.keys(value)
                .sort()
                .map((k) => [k, normalise(value[k], k)]),
        )
    }
    if ((key === 'ts' || key === 'timestamp') && Number(value) > SERVER_CLOCK_MICROS) return '<now>'
    return value
}

const [real, emulator] = process.argv.slice(2).map((path) => normalise(JSON.parse(readFileSync(path, 'utf8'))))
const dir = mkdtempSync(join(tmpdir(), 'parity-'))
const names = [...new Set([...Object.keys(real), ...Object.keys(emulator)])]
let differing = 0
for (const name of names) {
    const [want, got] = [real[name], emulator[name]].map((v) => JSON.stringify(v ?? null, null, 2) + '\n')
    if (want === got) continue
    differing++
    writeFileSync(join(dir, 'real'), want)
    writeFileSync(join(dir, 'emulator'), got)
    const { stdout } = spawnSync('diff', ['-u', '--label', 'real', '--label', 'emulator', join(dir, 'real'), join(dir, 'emulator')], { encoding: 'utf8' })
    console.log(`--- ${name}\n${stdout}`)
}
rmSync(dir, { recursive: true })
console.log(`${names.length - differing} of ${names.length} cases match.`)
process.exit(differing ? 1 : 0)

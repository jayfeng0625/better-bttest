// Compare each emulator's case results with the real table's, printing a diff for each case that differs and then a
// table of every case. Exits 1 if any case differs on the first emulator. The others only report.
// Usage: node compare.mjs <dir> <real> <emulator>...
// Each target's results are the JSON files in <dir>/<target>.
import assert from 'node:assert'
import { readdirSync, readFileSync } from 'node:fs'

// Timestamps above this are the server's clock, as in ReadModifyWriteRow cells. The cases write timestamps far below it.
const SERVER_CLOCK_MICROS = 1e15

const blankServerTimestamps = (key, value) => ((key === 'ts' || key === 'timestamp') && Number(value) > SERVER_CLOCK_MICROS ? '<now>' : value)
const resultsOf = (dir) =>
    Object.assign({}, ...readdirSync(dir).map((file) => JSON.parse(readFileSync(`${dir}/${file}`, 'utf8'), blankServerTimestamps)))

const [dir, realName, ...emulatorNames] = process.argv.slice(2)
const real = resultsOf(`${dir}/${realName}`)
const emulators = emulatorNames.map((name) => ({ name, results: resultsOf(`${dir}/${name}`), matching: new Set() }))

const names = [...new Set([real, ...emulators.map((e) => e.results)].flatMap(Object.keys))].sort()
for (const emulator of emulators) {
    for (const name of names) {
        try {
            assert.deepStrictEqual(emulator.results[name] ?? null, real[name] ?? null)
            emulator.matching.add(name)
        } catch (err) {
            console.log(`--- ${emulator.name}: ${name} (+ ${emulator.name}, - ${realName})\n${err.message.split('\n').slice(3).join('\n')}`)
        }
    }
}

console.log(`\n| Case | ${emulatorNames.join(' | ')} |\n|---|${emulatorNames.map(() => '---|').join('')}`)
for (const name of names) {
    console.log(`| ${name} | ${emulators.map((e) => (e.matching.has(name) ? 'match' : 'differs')).join(' | ')} |`)
}
console.log()
for (const emulator of emulators) console.log(`${emulator.name}: ${emulator.matching.size} of ${names.length} cases match ${realName}.`)
process.exit(emulators[0].matching.size === names.length ? 0 : 1)

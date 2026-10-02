// Check that the real table has the families the cases assume. Exits 2 if not.
// Usage: node target.mjs <project> <instance> <table> <MIN family> <MAX family> <plain family>
import { connect } from './client.mjs'

const [projectId, instanceId, tableId, MIN, MAX, PLAIN] = process.argv.slice(2)
let metadata
try {
    ;[metadata] = await connect(projectId, instanceId, tableId).table.getMetadata({ view: 'SCHEMA_VIEW' })
} catch (err) {
    console.error(`Cannot read the schema of ${tableId}: ${err.message}`)
    process.exit(2)
}
const families = metadata.columnFamilies

const problems = []
for (const [family, aggregator] of [
    [MIN, 'min'],
    [MAX, 'max'],
]) {
    const aggregate = families[family]?.valueType?.aggregateType
    if (!aggregate?.inputType?.int64Type || aggregate.aggregator !== aggregator) {
        problems.push(`${family} must be an int64 ${aggregator.toUpperCase()} aggregate family, got ${JSON.stringify(families[family] ?? null)}`)
    }
}
if (!families[PLAIN] || families[PLAIN].valueType) {
    problems.push(`${PLAIN} must be a family with no value type, got ${JSON.stringify(families[PLAIN] ?? null)}`)
}
// run.sh gives the emulators' families GC rule never, so the real table's need it too.
for (const family of [MIN, MAX, PLAIN]) {
    if (families[family]?.gcRule?.rule) {
        problems.push(`${family} must have GC rule never, got ${JSON.stringify(families[family].gcRule)}`)
    }
}
if (problems.length) {
    console.error(problems.join('\n'))
    process.exit(2)
}

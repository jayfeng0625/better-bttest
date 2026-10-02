// Check that the real table has the families the cases assume. Exits 2 if not.
// Usage: node target.mjs <project> <instance> <table> <aggregate family> <plain family>
import { connect } from './client.mjs'

const [projectId, instanceId, tableId, AGG, PLAIN] = process.argv.slice(2)
const [metadata] = await connect(projectId, instanceId, tableId).table.getMetadata({ view: 'SCHEMA_VIEW' })
const families = metadata.columnFamilies

const problems = []
const aggregate = families[AGG]?.valueType?.aggregateType
if (!aggregate?.inputType?.int64Type || aggregate.aggregator !== 'min') {
    problems.push(`${AGG} must be an int64 MIN aggregate family, got ${JSON.stringify(families[AGG] ?? null)}`)
}
if (!families[PLAIN] || families[PLAIN].valueType) {
    problems.push(`${PLAIN} must be a family with no value type, got ${JSON.stringify(families[PLAIN] ?? null)}`)
}
if (problems.length) {
    console.error(problems.join('\n'))
    process.exit(2)
}

const durationUnits = { ns: 0.000001, us: 0.001, ms: 1, s: 1000, m: 60000, h: 3600000 };

export function durationMilliseconds(value, name, allowZero = false) {
  if (typeof value !== "string" || !/^(\d+(?:\.\d+)?(?:ns|us|ms|s|m|h))+$/.test(value)) {
    throw new Error(`${name} requires an elapsed duration such as 24h`);
  }
  let result = 0;
  for (const part of value.matchAll(/(\d+(?:\.\d+)?)(ns|us|ms|s|m|h)/g)) {
    result += Number(part[1]) * durationUnits[part[2]];
  }
  if (!Number.isSafeInteger(result) || result < 0 || (!allowZero && result === 0)) {
    throw new Error(`${name} requires a positive whole number of milliseconds`);
  }
  return result;
}

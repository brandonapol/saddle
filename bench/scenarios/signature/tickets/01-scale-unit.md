# units: Scale takes a unit name

Difficulty: mechanical.

Change `units.Scale` to `Scale(v int, unit string)`, where "double" multiplies
by 2, and update the caller in `label`.

claims: units/**, label/**

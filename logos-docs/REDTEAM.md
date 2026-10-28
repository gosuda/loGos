# Red-team test ledger

Resolutions: logos-docs/SPEC.md Section 17. Status values: planned, tested, satisfied upstream, blocked.

| ID | Milestone | Test | Status |
| --- | --- | --- | --- |
| #1 |M2| test/logos/redteam/attack_001_split_union_ptr_scalar | tested |
| #2 | M5 | test/logos/run/zerocopy.lgo | tested |
| #3 | M5 | test/logos/run/constmap_switch.lgo; src/cmd/compile/internal/logos/constmap_test.go (TestConstmapSwitchMax) | tested |
| #4 | M18 | src/cmd/go/testdata/script/logos_redteam_4.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_4$'` passes both legs, the five arena park tests plus the `-race` leg SPEC 17 requires) | tested |
| #5 | M8, M20 | src/cmd/cgo/internal/testlogos/fastcall_test.go | tested |
| #6 | M6 | test/logos/run/lambda.lgo; test/logos/run/chains.lgo (dblBlock, count with ?) | tested |
| #7 | M3, M11 | test/logos/run/json_unmarshal_test.lgo; src/internal/types/testdata/logos/nonnil_zero.lgo; src/internal/types/testdata/logos/map_index.lgo | tested |
| #8 | M3 | test/logos/run/platform.lgo | tested |
| #9 | M13 | test/logos/redteam/attack_009_cold_last_trace_names.lgo (executed 2026-10-28: the fixpoint strip in src/cmd/link/internal/ld/pcln_logos.go:50-79 lands it; the fixture now asserts both suffixes absent and the base name present, and passes) | tested |
| #10 | M9 | test/logos/lint/exhaustive_union_switch.lgo; src/cmd/go/testdata/script/logos_pedantic_lint.txt | tested |
| #11 | M18 | test/logos/redteam/attack_011_preempt_arena_reachable.lgo | tested |
| #12 | M18 | test/logos/codegen/redteam12_bump_scan_guard.lgo | tested |
| #13 | M8, M9 | test/logos/lint/concurrent_multiword_race.lgo; test/logos/lint/concurrent_multiword_race_go.go | tested |
| #14 | M3 | src/internal/types/testdata/logos/narrowing.lgo | tested |
| #15 | M13 | test/logos/codegen/redteam15_megamorphic_limit.lgo | tested |
| #16 | M5 | src/internal/types/testdata/logos/const_views.lgo (cvPlainReslice, cvPlainReuseLoop); test/logos/run/reslice_reuse.lgo | tested |
| #17 | M6 | test/logos/run/tuples.lgo (direct) | tested |
| #18 |M1| test/logos/redteam/attack_018_shadowed_named_return | tested |
| #19 | M7 | src/internal/types/testdata/logos/viewas.lgo; test/logos/run/viewas.lgo | tested |
| #20 | M18 | test/logos/run/redteam20_arena_stack_ref.lgo | tested |
| #21 | M8 | src/logos/sync/netidle_logos_test.go (576.8 B per idle connection) | tested |
| #22 | M15 | test/logos/run/simd_slice_tail.lgo (page-end mmap); test/logos/codegen/simd_slice.lgo; src/simd/archsimd/logos_maskload_linux_amd64_test.go; src/cmd/compile/internal/logos/simdslice_test.go (no padding) | tested |
| #23 | M3 | src/internal/types/testdata/logos/error_first.lgo | tested |
| #24 | M5 | src/internal/types/testdata/logos/ctfe.lgo (BadMapRange); test/logos/run/const_map.lgo (sorted range) | tested |
| #25 | M12 | src/cmd/go/testdata/script/logos_cap_unsafe.txt | tested |
| #26 | M8 | src/logos/sync/sync_test.go (TestHotUpdateConcurrent, TestHotLayout) | tested |
| #27 | M16 | src/cmd/go/testdata/script/logos_wasm_loader.txt | tested |
| #28 | M13 | test/logos/redteam/attack_028_self_aliased_rvo.lgo | tested |
| #29 | M19 | test/logos/redteam/attack_029_zgc_syscall_untag.lgo | tested |
| #30 | M13 | test/logos/redteam/attack_030_static_devirtualization.lgo | tested |
| #31 | M6 | src/internal/types/testdata/logos/chains.lgo; test/logos/run/sort_side_effect.lgo | tested |
| #32 |M1| test/logos/redteam/attack_032_defer_lifo_order | tested |
| #33 | M17, M18 | test/logos/redteam/attack_033_stack_buffer_overflow.lgo | tested |
| #34 | M6 | test/logos/run/interp.lgo (bigBox AllocsPerRun) | tested |
| #35 | M18 | test/logos/run/redteam35_arena_race_reuse.lgo | tested |
| #36 |M2| test/logos/redteam/attack_036_open_union_value | tested |
| #37 | M7 | src/cmd/go/testdata/script/logos_eject_const_switch.txt | tested |
| #38 | M6 | src/cmd/compile/internal/syntax/parser_paren_logos_test.go | tested |
| #39 | M6 | test/logos/run/tuples.lgo (pair call unpack, tuple var pack) | tested |
| #40 |M2| test/logos/redteam/attack_040_union_equality | tested |
| #41 | M5, M6 | src/internal/types/testdata/logos/chains.lgo (chainMutateRecv, chainMutateParam) | tested |
| #42 |M1| test/logos/redteam/attack_042_defer_close_join | tested |
| #43 |M2| test/logos/redteam/attack_043_zero_sized_member | tested |
| #44 | M3 | src/cmd/go/testdata/script/logos_facts.txt | tested |
| #45 | M7 | src/internal/types/testdata/logos/const_switch.lgo; test/logos/run/const_switch.lgo | tested |
| #46 | M18 | test/logos/run/redteam46_arena_gc_scan.lgo | tested |
| #47 | M5 | src/internal/types/testdata/logos/ctfe.lgo | tested |
| #48 | M13 | test/logos/run/redteam48_generify_funcvalue.lgo | tested |
| #49 | M3 | test/logos/run/option_union.lgo | tested |
| #50 | M5 | test/logos/run/rodata_reflect.lgo | tested |
| #51 | M5 | src/internal/types/testdata/logos/ctfe.lgo | tested |
| #52 | M6 | src/cmd/compile/internal/logos/m6gaps_test.go (TestScopeDeferOpenCoded) | tested |
| #53 | M18 | test/logos/run/redteam53_recursive_arena_return.lgo | tested |
| #54 | M5 | test/logos/run/relro_write.lgo | tested |
| #55 |M1| test/logos/redteam/attack_055_errtrace_reset_cap | tested |
| #56 | M6 | test/logos/run/scope_defer.lgo (funcVarDefer, recvDefer) | tested |
| #57 |M2, M7| test/logos/redteam/attack_057_case_first_match | tested |
| #58 | M13 | src/cmd/go/testdata/script/logos_redteam_58.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_58$'` passes) | tested |
| #59 | M18 | src/cmd/go/testdata/script/logos_redteam_59.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_59$'` passes, race leg included) | tested |
| #60 | M3 | src/internal/types/testdata/logos/nonnil_zero.lgo | tested |
| #61 | M3 | test/logos/run/option_nested.lgo | tested |
| #62 | M5 | src/internal/types/testdata/logos/const_views.lgo (Rename, Peek) | tested |
| #63 | M18 | src/cmd/go/testdata/script/logos_redteam_63.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_63$'` passes, race leg included) | tested |
| #64 | M6 | test/logos/run/interp.lgo (nextID once, tag order) | tested |
| #65 | M7 | test/logos/run/const_for.lgo; src/cmd/go/testdata/script/logos_comptime_cross.txt | tested |
| #66 |M1| test/logos/redteam/attack_066_errdefer_unnamed_results | tested |
| #67 | M6, M13 | M6 half: src/cmd/compile/internal/logos/m6gaps_test.go (TestChainSumPairwise); test/logos/run/chains.lgo (sum order). M13: (planned: no dedicated redteam67 attack file; cross-arch FMA sum-drift attack missing, arm64 leg still uncovered) | planned |
| #68 | M8 | src/logos/rt/group_test.go (FirstErrorWins, CancelReachesSibling); test/logos/run/group_exit.lgo | tested |
| #69 | M10 | src/cmd/go/testdata/script/logos_next.txt; src/cmd/compile/internal/logos/next_test.go | tested |
| #70 | M5 | test/logos/codegen/rodata.lgo | tested |
| #71 | M18 | src/cmd/go/testdata/script/logos_redteam_71.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_71$'` passes, race leg included) | tested |
| #72 | M7 | test/logos/run/viewas.lgo (GC keep-alive, need-have panic); src/logos/rt/viewas_test.go | tested |
| #73 | M5 | test/logos/run/const_map.lgo; src/cmd/compile/internal/logos/constmap_test.go | tested |
| #74 | M13 | test/logos/redteam/attack_074_funcforpc_spec_name.lgo | tested |
| #75 |M2| test/logos/redteam/attack_075_nil_arm_common_type | tested |
| #76 | M7 | test/logos/compile/const_for_perfield.lgo; test/logos/compile/const_for_error.lgo; test/logos/run/const_for_lines.lgo | tested |
| #77 | M9 | test/logos/lint/defer_erases_errdefer_state.lgo | tested |
| #78 | M16 | test/logos/codegen/wasm_align.lgo | tested |
| #79 |M1| test/logos/redteam/attack_079_import_alias_collision | tested |
| #80 | M22 | src/cmd/go/testdata/script/logos_redteam_80.txt (planned: script exists, no executed test/logos attack run) | planned |
| #81 | M18 | src/cmd/go/testdata/script/logos_redteam_81.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_81$'` passes, race leg included) | tested |
| #82 |M2| test/logos/redteam/attack_082_union_gc_shape | tested |
| #83 | M5 | test/logos/run/zerocopy.lgo | tested |
| #84 | M6 | src/cmd/compile/internal/logos/m6gaps_test.go (TestChainFuseOneLoop) | tested |
| #85 |M1| test/logos/redteam/attack_085_typed_nil_errdefer | tested |
| #86 | M7 | src/internal/types/testdata/logos/const_switch.lgo; test/logos/run/const_switch.lgo | tested |
| #87 |M1| test/logos/redteam/attack_087_errdefer_panic_goexit | tested |
| #88 | M20 | src/cmd/cgo/internal/testlogos/fastcall_test.go | tested |
| #89 |M2| test/logos/redteam/attack_089_scalar_roundtrip | tested |
| #90 |M1| test/logos/redteam/attack_090_errdefer_recover_success_skip | tested |
| #91 | M7 | test/logos/run/const_switch.lgo; src/internal/types/testdata/logos/const_switch.lgo | tested |
| #92 | M13 | test/logos/codegen/redteam92_polymorphic_recursion.lgo | tested |
| #93 | M5 | test/logos/run/constview_bound.lgo | tested |
| #94 | M10 | src/cmd/go/testdata/script/logos_context.txt; src/cmd/compile/internal/logos/context_test.go (TestContextLineCap); src/cmd/compile/internal/logos/context_bench_test.go (BenchmarkContext) | tested |
| #95 | M6 | test/logos/run/chains.lgo (take, drop, get clamps) | tested |
| #96 |M1| test/logos/redteam/attack_096_errdefer_armed_flag | tested |
| #97 |M2| test/logos/redteam/attack_097_struct_variant_slots | tested |
| #98 | M5 | test/logos/run/rodata_reflect.lgo | tested |
| #99 | M3 | src/internal/types/testdata/logos/map_index.lgo | tested |
| #100 | M15 | src/internal/types/testdata/logos/simd_types.lgo (simdTooLarge); test/logos/run/simd.lgo | tested |
| #101 | M18 | test/logos/run/redteam101_arena_race_reuse.lgo | tested |
| #102 | M5 | src/internal/types/testdata/logos/ctfe.lgo | tested |
| #103 | M5 | src/internal/types/testdata/logos/ctfe.lgo | tested |
| #104 | M13 | test/logos/codegen/cold_frame_pointer.lgo | tested |
| #105 | M13 | test/logos/redteam/attack_105_no_cold_section.lgo | tested |
| #106 | M18 | src/cmd/go/testdata/script/logos_redteam_106.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_106$'` passes, race leg included) | tested |
| #107 | M13 | src/cmd/go/testdata/script/logos_opt_levels.txt | tested |
| #108 | M7 | src/cmd/go/testdata/script/logos_eject_const_switch.txt; src/cmd/compile/internal/logos (TestEjectMatchesLowering) | tested |
| #109 | M10 | src/cmd/go/testdata/script/logos_next.txt (goldens, ! logos fmt) | tested |
| #110 | M5 | test/logos/run/freeze.lgo | tested |
| #111 | M15 | test/logos/codegen/arm64_pair.lgo; test/logos/codegen/arm64_pair.go; src/cmd/compile/internal/ssa/pair_logos_test.go (TestLogosPairARM64) | tested |
| #112 | M1 | src/cmd/go/testdata/script/logos_eject.txt | tested |
| #113 | M6 | src/cmd/compile/internal/logos/m6gaps_test.go (TestScopeDeferOpenCoded) | tested |
| #114 | M7 | test/logos/run/const_for.lgo | tested |
| #115 | M3 | src/internal/types/testdata/logos/union_exhaustive.lgo | tested |
| #116 | M4 | src/internal/types/testdata/logos/property_iface.lgo | tested |
| #117 | M4 | src/internal/types/testdata/logos/property_iface.lgo | tested |
| #118 | M4 | src/internal/types/testdata/logos/property_iface.lgo | tested |
| #119 | M6 | test/logos/run/spread.lgo; src/internal/types/testdata/logos/spread.lgo | tested |
| #120 | M4 | test/logos/run/properties.lgo | tested |
| #121 | M13 | test/logos/run/redteam121_const_slice.lgo; test/logos/codegen/redteam121_no_copy.lgo | tested |
| #122 | M0 | src/cmd/go/testdata/script/logos_alias.txt | tested |
| #123 | M6, M11 | test/logos/run/json_unmarshal_test.lgo (TestDefaultsAppliedFirst) | tested |
| #124 | M13 | test/logos/run/redteam124_type_assert.lgo | tested |
| #125 |M1| test/logos/redteam/attack_125_errdefer_join | tested |
| #126 | M6 | src/cmd/compile/internal/logos/m6gaps_test.go (TestInterpBindBeforeSizing) | tested |
| #127 | M4 | src/internal/types/testdata/logos/property_iface.lgo | tested |
| #128 | M7 | test/logos/run/comptime_generic.lgo; test/logos/run/comptime_generic_inner_lowering.lgo; src/cmd/go/testdata/script/logos_comptime_cross.txt; src/cmd/go/testdata/script/logos_comptime_importcfg.txt; src/cmd/go/testdata/script/logos_comptime_foreign_types.txt | tested |
| #129 | M7 | test/logos/compile/comptime_cycle.lgo | tested |
| #130 | M7 | test/logos/compile/comptime_mapto.lgo; test/logos/run/comptime_mapto.lgo | tested |
| #131 | M4 | src/internal/types/testdata/logos/property_iface.lgo | tested |
| #132 | M4 | test/logos/run/properties.lgo | tested |
| #133 | M0 | test/logos/lint/alias_extension.lgo; src/cmd/go/testdata/script/logos_lint_alias_extension.txt | tested |
| #134 | M13 | test/logos/codegen/recursive_stack.lgo; test/logos/codegen/recursive_stack.go; test/logos/codegen/inline_levels.lgo; src/cmd/compile/internal/inline/stackbuf_logos_test.go (TestLogosVariableMakeThreshold) | tested |
| #135 |M1| test/logos/redteam/attack_135_lifo_order | tested |
| #136 | M8 | test/logos/run/group_chan.lgo | tested |
| #137 | M15 | test/logos/run/wasm_loop_preempt.lgo | tested |
| #138 | M18 | test/logos/run/redteam138_sync_pool.lgo; test/logos/codegen/redteam138_pool_heap.lgo | tested |
| #139 | M11 | test/logos/run/json_unmarshal_test.lgo (TestHostileArrayCostsOnArrival, TestLargeArrayDecodesIncrementally) | tested |
| #140 | M5 | src/cmd/compile/internal/logos/constmap_test.go (TestConstmapBudgetIsOneMillion, TestConstmapProbeIsSingleProbe); src/logos/rt/constmap_proof_test.go (TestConstTableGetSingleProbe) | tested |
| #141 | M4 | test/logos/run/properties.lgo | tested |
| #142 | M6 | test/logos/run/chains.lgo (at-return closes) | tested |
| #143 | M8 | src/logos/sync/chanslot_logos_test.go | satisfied upstream |
| #144 | M17, M20 | src/cmd/go/testdata/script/logos_redteam_144.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_144$'` passes) | tested |
| #145 | M4 | src/internal/types/testdata/logos/property_iface.lgo | tested |
| #146 | M6 | src/cmd/compile/internal/logos/spread_test.go; test/logos/codegen/spread_nomemmove.lgo | tested |
| #147 | M8 | src/logos/rt/group_test.go; test/logos/run/group_panic.lgo | tested |
| #148 | M8, M9 | test/logos/lint/concurrent_multiword_race.lgo; test/logos/lint/concurrent_multiword_race_go.go | tested |
| #149 | M15 | test/logos/codegen/clear_kept.lgo | satisfied upstream |
| #150 | M7 | src/cmd/go/testdata/script/logos_comptime_nodes.txt | tested |
| #151 | M8 | test/logos/run/group_netpoll.lgo; src/runtime/logos_group_netpoll_test.go | tested |
| #152 | M5 | test/logos/run/zerocopy.lgo (other-provenance AllocsPerRun, survival) | tested |
| #153 | M8 | src/logos/sync/sync_test.go (TestHotNoMemoryGrowth) | tested |
| #154 | M8 | src/logos/sync/sync_test.go (TestHotLayout) | tested |
| #155 | M18 | test/logos/codegen/redteam155_bump_unsafe.lgo | tested |
| #156 | M3 | src/internal/types/testdata/logos/error_first.lgo | tested |
| #157 | M7, M9 | test/logos/lint/infinite_recursion.lgo; test/logos/lint/infinite_recursion_go.go | tested |
| #158 | M9 | test/logos/lint/large_backing_array_pin.lgo; test/logos/lint/large_backing_array_pin_go.go | tested |
| #159 | M19 | src/cmd/go/testdata/script/logos_redteam_159.txt (blocked: M19-S5 immix phase 1, and zgc phase 2 for the pointer-keyed map half; scripts exist, no executed run) | blocked |
| #160 | M19, M20 | src/cmd/go/testdata/script/logos_redteam_160.txt (blocked: M19-S5 immix phase 1; the script links immix explicitly, no executed run) | blocked |
| #161 | M19 | src/runtime/logos_pacer_test.go (TestLogosElasticKeepsAssistBackstop, assists still run at u >= 0.95) | tested |
| #162 | M9 | src/cmd/go/testdata/script/logos_lint_go.txt | tested |
| #163 | M9 | test/logos/lint/unbounded_channel_op.lgo; test/logos/lint/unbounded_channel_op_go.go | tested |
| #164 | M9 | test/logos/lint/discarded_error.lgo; test/logos/lint/discarded_error_go.go | tested |
| #165 | M20 | src/cmd/cgo/internal/testlogos/fastcall_test.go | tested |
| #166 | M19 | src/runtime/logos_gcmode_test.go (TestLogosGCModeBatchTriggersAtNinetyPercent, TestLogosGCModeBatchHeapCorrect) | tested |
| #167 | M8, M9 | test/logos/lint/reentrant_withlock_deadlock.lgo; test/logos/lint/reentrant_withlock_deadlock_go.go; src/logos/sync/sync_test.go (TestLockedWithLockMutates, TestLockedWithLockUnlocksAfterPanic) | tested |
| #168 | M19 | src/cmd/go/testdata/script/logos_redteam_168.txt (blocked: M19-S7 zgc phase 2; the script links zgc explicitly, no executed run) | blocked |
| #169 | M15 | test/logos/run/make_overflow.lgo (length and capacity); test/logos/run/make_overflow_go.go; test/logos/run/make_const.lgo; src/logos/rt/mullen_test.go | tested |
| #170 | M9 | test/logos/lint/time_after_in_loop.lgo; test/logos/lint/time_after_in_loop_go.go | tested |
| #171 | M19 | src/cmd/go/testdata/script/logos_redteam_171.txt (blocked: M19-S7 zgc phase 2; the script links zgc explicitly, no executed run) | blocked |
| #172 | M9 | test/logos/lint/time_after_in_loop.lgo; test/logos/lint/time_after_in_loop_go.go | tested |
| #173 | M20 | src/cmd/cgo/internal/testlogos/fastcall_test.go | tested |
| #174 | M7 | src/internal/types/testdata/logos/const_if.lgo; test/logos/run/const_if.lgo; test/logos/codegen/const_if_dead.lgo | tested |
| #175 | M19 | src/cmd/go/testdata/script/logos_redteam_175.txt (blocked: M19-S5 immix phase 1; the script links immix explicitly, no executed run) | blocked |
| #176 | M8 | src/logos/sync/sync_test.go (TestLockedWithLockUnlocksAfterPanic) | tested |
| #177 | M6 | test/logos/run/interp.lgo (format verbs printed literally) | tested |
| #178 | M8 | src/logos/rt/group_test.go; test/logos/run/group.lgo; src/internal/types/testdata/logos/group.lgo | tested |
| #179 | M6 | test/logos/run/chains.lgo (64-item stability, NaN order) | tested |
| #180 | M5 | src/cmd/compile/internal/logos/ctfe/ctfe_test.go (TestWidthsComposite); test/logos/run/const_init_read.lgo | tested |
| #181 | M18 | src/cmd/go/testdata/script/logos_redteam_181.txt (executed 2026-10-28: `go test cmd/go -run 'Script/^logos_redteam_181$'` passes, race leg included) | tested |
| #182 | M9 | test/logos/lint/serialized_unsized_int.lgo | tested |
| #183 | M0 | src/cmd/go/testdata/script/logos_interop.txt (case 1) | tested |
| #184 | M0 | src/cmd/go/testdata/script/logos_interop.txt (case 2) | tested |
| #185 | M2 | test/logos/redteam/attack_185_dual_use_union; src/cmd/go/testdata/script/logos_eject_union.txt; src/cmd/go/testdata/script/logos_interop_union.txt | tested |
| #186 | M3 | src/cmd/go/testdata/script/logos_interop_nil.txt | tested |
| #187 | M16 | src/cmd/go/testdata/script/logos_wasm_loader.txt | tested |
| #188 | M20 | src/cmd/cgo/internal/testlogos/fastcall_test.go | tested |
| #189 | M20 | src/cmd/cgo/internal/testlogos/fastcall_test.go | tested |
| #190 | M5 | test/logos/compile/const_interface_generic_assert.lgo; test/logos/run/const_interface_generic_assert.lgo; src/cmd/go/testdata/script/logos_const_interface_generic_assert.txt (local and imported function/receiver instantiations, wrappers, closures, type switches, composite/nested targets, and parameter indexes above 64) | planned |

Generic assertion effects have source changes, not a completed parent verification gate. Ordinary assertion definitions remain legal; the compile fixture targets unsafe qualified instantiations, while the run fixture retains identity, ordinary named arguments, and known-zero storage-container controls. The script covers both Go-to-loGo/s and loGo/s-to-Go generic imports. Receiver-readonly facts must not stand in for parameter-mutation summaries, and neither assertion nor mutation masks may truncate positions at or above 64.

The parent reported that the corrected baseline unsafe fixture compiled unexpectedly and that the safe run controls passed. Those are baseline observations, not evidence that the new source closes the attacks. Pending parent gates include a rebuilt compiler, the compile-negative and safe-run fixtures, the cross-package script, facts-v3/unknown-versus-known-zero and arbitrary-arity assertion/mutation cases, and the parent-owned broader regression gates. No generic assertion attack is marked tested or closed here.

The source owner's disclosed limits remain open for parent review: CTFE-driven instantiations after checker cleanup are not validated by the recorded cleanup pass; exported per-parameter masks collapse direct and nested assertion targets, and imported declarations retain masks rather than local site evidence. Reflection and `unsafe` remain outside this model.

## Cgo benchmark input validity

The old `BenchmarkFastcall` recurrence caused signed C overflow.
A 64-call UBSan run failed at `fastcall_c.c:12` on `2147483647 + 1`.
Both cgo benchmarks now bound the first argument to 1073741823 and assign the C result.
The maximum C sum is 1073741824, within the signed 32-bit range.
Both variants pass the same UBSan check with `logosfastcgo` enabled and disabled.
The C operation and the performance thresholds are unchanged.
Historical timings from the old recurrence are observations of a different workload.
They do not prove acceptance of the corrected benchmark.

Execution boundary (M20): the fastcall/M-owned, occupancy/dirty, and foreign-contract source changes in this worktree are authored/implemented but UNVERIFIED. Historical `tested` rows are preserved only where their executed evidence supports them; retained failures, skips, and blocked/noise-limited checks are not passes, and no row is flipped to `tested` by code reading. The M20 acceptance gate (`-race`, each passing M19 GC mode, G6 speed ratio of at least 1.05, blocking parity, command-default separation, `./run.bash --no-rebuild` with the plain-Go default unchanged) and the M19 phase 1/2 mode gates stay closed. Rows #159, #160, #168, #171, #175 stay `planned` (unmet); no unmet attack is closed here and no speedup is claimed.

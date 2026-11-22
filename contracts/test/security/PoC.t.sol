// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {SecBase} from "./Base.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {SeshRegistry} from "../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../src/exits/ExitLease.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";

/// Proofs of concept for the findings in test/security/README.md. On this
/// branch H-1 and H-2 are fixed and their tests check the attack now fails;
/// M-1 (a design trade-off) still shows the attacker's gain.
contract SecurityPoC is SecBase {
    XmrSwapV3 v3;
    SeshRegistry reg;
    ExitLease el;
    MockUSDC usdc;

    uint256 makerPk = 0x3A4E5;
    address maker;
    address taker = address(0xA11CE);
    address claimer = address(0xC1A1);
    uint256 lid;
    uint64 constant D1 = 1 hours;
    uint64 constant D2 = 1 hours;
    uint64 constant G = 30 minutes;
    string constant MSESH = "maker1.sesh";

    /// Warp past the V3 lock-in window (cancelIntent is refused before it).
    function _li(XmrSwapV3 k, XmrSwapV3.Swap memory s) internal {
        (, uint64 d,) = k.status(k.swapId(s));
        if (block.timestamp < d) vm.warp(d);
    }

    function setUp() public {
        vm.warp(1_000_000);
        (v3, reg, el, usdc) = deployAll();
        maker = vm.addr(makerPk);
        vm.deal(maker, 10 ether);
        vm.deal(taker, 10 ether);
        vm.prank(maker);
        lid = reg.register{value: MAKER_BOND}(MSESH, "maker", "");
    }

    function _lock(uint256 value, bytes32 salt) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = XmrSwapV3.Swap(
            taker, claimer, BOB_K, ALICE_K,
            uint64(block.timestamp) + D1, uint64(block.timestamp) + D1 + D2, G,
            address(0), value, 0, salt
        );
        vm.prank(taker);
        id = v3.lock{value: value}(claimer, BOB_K, ALICE_K, D1, D2, G, address(0), value, 0, salt);
    }

    function _commitment(uint256 value) internal view returns (SeshRegistry.Commitment memory c) {
        c = SeshRegistry.Commitment(
            lid, taker, claimer, BOB_K, address(0), value, 1, MIN_PEN, ATTEST, uint64(block.timestamp) + 15 minutes
        );
    }

    function _sign(SeshRegistry.Commitment memory c) internal view returns (uint8 v, bytes32 r, bytes32 s) {
        return vm.sign(makerPk, reg.commitmentDigest(c));
    }

    /// H-1 (fixed): one signed Commitment used to be recordable against many
    /// V3 swaps with the same terms and different salts. Now each signed
    /// commitment is accepted once.
    function test_PoC_H1_CommitmentReplayAcrossSalts() public {
        uint256 value = 0.001 ether;
        SeshRegistry.Commitment memory c = _commitment(value);
        (uint8 v, bytes32 r, bytes32 ss) = _sign(c); // maker signs ONCE
        (XmrSwapV3.Swap memory s1,) = _lock(value, bytes32(uint256(1)));
        reg.commit(s1, c, v, r, ss);
        (XmrSwapV3.Swap memory s2,) = _lock(value, bytes32(uint256(2)));
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.commit(s2, c, v, r, ss);
        assertEq(reg.listing(lid).openCommits, 1);
        assertEq(reg.listing(lid).reserved, MIN_PEN);
    }

    /// H-2 (fixed): the taker used to lock, commit and cancel at once (so the
    /// maker never locks XMR and never acks) and then slash. Now V3 refuses a
    /// cancel inside the lock-in window, and a cancel made before attestBy
    /// (possible when the attest window is longer than LOCK_IN) blocks the
    /// slash, and so does a refund.
    function test_PoC_H2_CancelThenSlash() public {
        uint256 value = 0.001 ether;
        SeshRegistry.Commitment memory c = _commitment(value);
        c.attestWindow = 30 minutes; // longer than LOCK_IN
        (uint8 v, bytes32 r, bytes32 ss) = _sign(c);
        (XmrSwapV3.Swap memory s, bytes32 id) = _lock(value, bytes32(uint256(1000)));
        reg.commit(s, c, v, r, ss);
        vm.prank(taker);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        v3.cancelIntent(s); // same block as the lock: inside the lock-in window
        _li(v3, s);
        vm.prank(taker);
        v3.cancelIntent(s); // at lock-in end, still before attestBy
        vm.warp(block.timestamp + 11 minutes); // past attestBy, inside the cancel freeze
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.slash(id); // CANCELLING, cancelled before attestBy
        vm.prank(taker);
        v3.refund(s, ALICE_S);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.slash(id); // REFUNDED
        assertEq(reg.listing(lid).bond, MAKER_BOND);
    }

    /// A real no-show is still slashable: no ack by attestBy, the taker then
    /// cancels (after attestBy) and slashes before it refunds.
    function test_H2_NoShowStillSlashableAfterLateCancel() public {
        uint256 value = 0.001 ether;
        SeshRegistry.Commitment memory c = _commitment(value);
        (uint8 v, bytes32 r, bytes32 ss) = _sign(c);
        (XmrSwapV3.Swap memory s, bytes32 id) = _lock(value, bytes32(uint256(1001)));
        reg.commit(s, c, v, r, ss);
        vm.warp(block.timestamp + ATTEST);
        _li(v3, s);
        vm.prank(taker);
        v3.cancelIntent(s);
        uint256 before = taker.balance;
        reg.slash(id);
        assertEq(taker.balance - before, MIN_PEN);
    }

    /// Exact boundary: a cancel at attestBy - 1 blocks the slash, one at attestBy does not.
    function testFuzz_H2_CancelTimingBoundary(uint8 off) public {
        uint256 value = 0.001 ether;
        SeshRegistry.Commitment memory c = _commitment(value);
        c.attestWindow = 30 minutes; // attestBy after the lock-in end, so both sides of it are reachable
        (uint8 v, bytes32 r, bytes32 ss) = _sign(c);
        (XmrSwapV3.Swap memory s, bytes32 id) = _lock(value, bytes32(uint256(2000)));
        reg.commit(s, c, v, r, ss);
        (, uint64 attestBy,,,,) = reg.commits(id);
        uint256 tc = uint256(attestBy) - 3 + (off % 6);
        vm.warp(tc);
        vm.prank(taker);
        v3.cancelIntent(s);
        vm.warp(attestBy > tc ? attestBy : tc);
        if (tc < attestBy) {
            vm.expectRevert(SeshRegistry.WrongStatus.selector);
            reg.slash(id);
        } else {
            reg.slash(id);
        }
    }

    function _premiumSwap(bytes32 salt) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = XmrSwapV3.Swap(
            taker, vm.addr(M1_BOB), BOB_K, ALICE_K, uint64(block.timestamp) + D1, uint64(block.timestamp) + D1 + D2, G,
            address(0), 1 ether, 0.01 ether, salt
        );
        vm.prank(taker);
        id = v3.lock{value: 1.01 ether}(s.claimer, BOB_K, ALICE_K, D1, D2, G, address(0), 1 ether, 0.01 ether, salt);
    }

    uint256 constant M1_BOB = 0xB0B5EC;
    address constant M1_SINK = address(0x5111C);

    /// M-1 (fixed with a lock-in window): the taker used to watch the maker's
    /// XMR arrive and front-run ackLocked with cancelIntent, keeping the
    /// premium. Now cancelIntent reverts until lock time + LOCK_IN, so an ack
    /// sent inside the window always lands first; a later cancel forfeits the
    /// premium to the maker.
    function test_PoC_M1_AckFrontRunKeepsPremium() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = _premiumSwap(bytes32(uint256(7)));
        uint256 lockInEnd = block.timestamp + v3.LOCK_IN();
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(M1_BOB, v3.ackDigest(id, M1_SINK));
        // the taker tries to front-run at once and at the last second of the window
        vm.prank(taker);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        v3.cancelIntent(s);
        vm.warp(lockInEnd - 1);
        vm.prank(taker);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        v3.cancelIntent(s);
        v3.ackLocked(s, M1_SINK, v, r, ss); // lands
        vm.warp(lockInEnd);
        vm.prank(taker);
        v3.cancelIntent(s); // allowed from exactly lockInEnd
        uint256 before = taker.balance;
        vm.prank(taker);
        v3.refund(s, ALICE_S);
        assertEq(taker.balance - before, 1 ether, "taker gets only the value back");
        assertEq(M1_SINK.balance, 0.01 ether, "premium goes to the maker");
    }

    /// Residual (by design): an ack sent after the window can still be
    /// front-run. swapd's maker acks as soon as its XMR lock is broadcast.
    function test_M1_AckAfterWindowCanStillBeFrontRun() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = _premiumSwap(bytes32(uint256(8)));
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(M1_BOB, v3.ackDigest(id, M1_SINK));
        _li(v3, s);
        vm.prank(taker);
        v3.cancelIntent(s);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        v3.ackLocked(s, M1_SINK, v, r, ss);
        vm.prank(taker);
        v3.refund(s, ALICE_S);
        assertEq(M1_SINK.balance, 0);
    }

    /// Boundary: cancelIntent works iff lock time + LOCK_IN <= now < t1.
    function testFuzz_M1_LockInBoundary(uint32 dt) public {
        uint256 t0 = block.timestamp;
        (XmrSwapV3.Swap memory s,) = _premiumSwap(bytes32(uint256(9)));
        uint256 t = t0 + bound(dt, 0, D1 + 10);
        vm.warp(t);
        vm.prank(taker);
        (bool ok,) = address(v3).call(abi.encodeCall(XmrSwapV3.cancelIntent, (s)));
        assertEq(ok, t >= t0 + v3.LOCK_IN() && t < s.t1);
        // the taker's funds are never trapped: a late refund always works
        if (!ok) {
            vm.warp(s.t2);
            vm.prank(taker);
            v3.refund(s, ALICE_S);
        }
    }

    /// Any ack inside the window, followed by a cancel at any later time it is
    /// allowed: the premium always goes to the maker.
    function testFuzz_M1_AckInWindowAlwaysPaysPremium(uint32 ackAt, uint32 cancelAt) public {
        uint256 t0 = block.timestamp;
        (XmrSwapV3.Swap memory s, bytes32 id) = _premiumSwap(bytes32(uint256(10)));
        vm.warp(t0 + bound(ackAt, 0, v3.LOCK_IN() - 1));
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(M1_BOB, v3.ackDigest(id, M1_SINK));
        v3.ackLocked(s, M1_SINK, v, r, ss);
        vm.warp(t0 + v3.LOCK_IN() + bound(cancelAt, 0, D1 - v3.LOCK_IN() - 1));
        vm.prank(taker);
        v3.cancelIntent(s);
        vm.prank(taker);
        v3.refund(s, ALICE_S);
        assertEq(M1_SINK.balance, 0.01 ether);
    }

    /// The window is a constant: shorter than the shortest t1 and the
    /// shortest grace, so it always ends before t1 and the claimer's window.
    function test_LockInBounds() public view {
        assertGt(v3.LOCK_IN(), 0);
        assertLt(v3.LOCK_IN(), v3.MIN_D1());
        assertLe(v3.LOCK_IN(), v3.MIN_GRACE());
    }
}

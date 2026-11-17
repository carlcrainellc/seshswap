// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../src/XmrSwapV3.sol";
import {Token, FalseToken, ReentrantToken, BombToken, RejectEth, UsdtLike} from "./Tokens.sol";

/// XmrSwapV3: the V2 behaviour that is kept, the two-step "freeze, then reveal"
/// claim and cancel, and the taker premium against the claimer's signed ack.
contract XmrSwapV3Test is Test {
    XmrSwapV3 c;
    address alice = address(0xA11CE);
    uint256 bobPk = 0xB0B5EC;          // test-only EOA key (claimer: signs intents, acks, relayed claims)
    address bob;
    address relayer = address(0x7E1A7);
    address sink = address(0x5111C);   // where the maker wants a forfeited premium
    // test-only ed25519 key shares (secret, LE-encoded public key) from swapd/cmd/edvec
    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    uint64 constant D1 = 1 hours;
    uint64 constant D2 = 2 hours;
    uint64 constant G = 6 hours;
    uint256 constant T0 = 1_000_000;
    uint256 constant P = 0.003 ether;  // premium

    function setUp() public {
        c = new XmrSwapV3();
        bob = vm.addr(bobPk);
        vm.deal(alice, 100 ether);
        vm.warp(T0);
    }

    // ------------------------------------------------------------ helpers
    function _swap(address asset, uint256 value, uint256 premium, bytes32 salt) internal view returns (XmrSwapV3.Swap memory) {
        return XmrSwapV3.Swap(alice, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1,
                              uint64(block.timestamp) + D1 + D2, G, address(0) == asset ? address(0) : asset, value, premium, salt);
    }

    function lockEthP(uint256 premium, bytes32 salt) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = _swap(address(0), 1 ether, premium, salt);
        vm.prank(alice);
        id = c.lock{value: 1 ether + premium}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), 1 ether, premium, salt);
        assertEq(id, c.swapId(s));
    }

    function lockEth() internal returns (XmrSwapV3.Swap memory s, bytes32 id) { return lockEthP(0, bytes32(uint256(1))); }

    function stage(bytes32 id) internal view returns (uint256) { (XmrSwapV3.Stage s,,) = c.status(id); return uint256(s); }
    function deadline(bytes32 id) internal view returns (uint64) { (, uint64 d,) = c.status(id); return d; }
    function premiumTo(bytes32 id) internal view returns (address) { (,, address a) = c.status(id); return a; }

    function intent(XmrSwapV3.Swap memory s, bytes32 id) internal {
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.prank(relayer); // anyone may submit the claimer's signed intent
        c.claimIntent(s, v, r, ss);
    }

    function ack(XmrSwapV3.Swap memory s, bytes32 id, address to) internal {
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.ackDigest(id, to));
        vm.prank(relayer);
        c.ackLocked(s, to, v, r, ss);
    }

    function signClaim(bytes32 id, address to, address rel, uint256 fee) internal view returns (uint8 v, bytes32 r, bytes32 s) {
        return vm.sign(bobPk, c.claimDigest(id, to, rel, fee));
    }

    // ------------------------------------------------------------ lock
    function test_LockStoresOneSlotAndEmitsSwap() public {
        XmrSwapV3.Swap memory s = _swap(address(0), 1 ether, 0, bytes32(uint256(1)));
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.Locked(c.swapId(s), s);
        (, bytes32 id) = lockEth();
        assertEq(stage(id), 1);
        assertEq(deadline(id), 0);
        assertEq(premiumTo(id), address(0));
        assertEq(address(c).balance, 1 ether);
    }

    function test_LockRejectsBadParams() public {
        vm.startPrank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(address(0), BOB_K, ALICE_K, D1, D2, G, address(0), 1, 0, 0);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 0}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), 0, 0, 0);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, BOB_K, D1, D2, G, address(0), 1, 0, 0);          // same key
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, 1, ALICE_K, D1, D2, G, address(0), 1, 0, 0);            // identity point
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, 1, D1, D2, G, address(0), 1, 0, 0);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 2}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), 1, 0, 0);        // msg.value mismatch
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), 1, 1, 0);        // premium not sent
        vm.expectRevert();
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), type(uint256).max, 2, 0); // value+premium overflow
        vm.expectRevert();
        c.lock{value: 1}(bob, BOB_K, ALICE_K, type(uint64).max, D2, G, address(0), 1, 0, 0); // t1 overflow
        vm.stopPrank();
    }

    function test_LockEnforcesMinimumWindowsAndGraceBounds() public {
        assertEq(c.MIN_D1(), 30 minutes);
        assertEq(c.MIN_D2(), 30 minutes);
        assertEq(c.MIN_GRACE(), 30 minutes);
        assertEq(c.MAX_GRACE(), 48 hours);
        vm.startPrank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, 30 minutes - 1, D2, G, address(0), 1, 0, 0);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, 30 minutes - 1, G, address(0), 1, 0, 0);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, D2, 30 minutes - 1, address(0), 1, 0, 0);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, D2, 48 hours + 1, address(0), 1, 0, 0);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, 30 minutes, 30 minutes, 30 minutes, address(0), 1, 0, 0);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, 30 minutes, 30 minutes, 48 hours, address(0), 1, 0, bytes32(uint256(2)));
        vm.stopPrank();
    }

    function test_LockDuplicateReverts() public {
        lockEth();
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.Exists.selector);
        c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), 1 ether, 0, bytes32(uint256(1)));
    }

    // ------------------------------------------------------------ two-step claim
    function test_ReadyThenIntentThenClaim() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        assertEq(stage(id), 2);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.ClaimIntent(id, uint64(T0) + G); // max(t2 = T0+3h, now+6h)
        intent(s, id);
        assertEq(stage(id), 5);
        assertEq(deadline(id), uint64(T0) + G);
        address to = address(0xF2E5);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.Claimed(id, BOB_S, to, address(0), 0);
        vm.prank(bob);
        c.claim(s, BOB_S, to);
        assertEq(to.balance, 1 ether);
        assertEq(stage(id), 3);
        assertEq(address(c).balance, 0);
    }

    function test_OneStepClaimRejected() public {
        (XmrSwapV3.Swap memory s,) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector); // no intent yet: the secret is never accepted
        c.claim(s, BOB_S, bob);
        vm.warp(s.t1);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        c.claim(s, BOB_S, bob);
    }

    function test_ClaimIntentAfterT1WithoutReady() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.warp(s.t1 - 1);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.claimIntent(s, v, r, ss);
        vm.warp(s.t1);
        c.claimIntent(s, v, r, ss);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
        assertEq(bob.balance, 1 ether);
    }

    function test_ClaimIntentDeadlineIsMaxOfT2AndGrace() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.warp(s.t1); // t2 - now = 2h < G
        intent(s, id);
        assertEq(deadline(id), uint64(s.t1) + G);
        // a swap with a long claim window: the deadline is t2
        XmrSwapV3.Swap memory s2 = XmrSwapV3.Swap(alice, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + 30 hours, G, address(0), 1 ether, 0, bytes32(uint256(2)));
        vm.prank(alice);
        bytes32 id2 = c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, 30 hours, G, address(0), 1 ether, 0, bytes32(uint256(2)));
        vm.prank(alice);
        c.setReady(s2);
        intent(s2, id2);
        assertEq(deadline(id2), s2.t2);
    }

    /// The point of the design: once the intent is in, the owner cannot refund until
    /// the deadline, even long after t2. The claimer reveals while frozen.
    function test_ClaimIntentFreezesRefundPastT2() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.warp(s.t2 - 1); // last second of the claimer's window
        intent(s, id);
        uint64 d = deadline(id);
        assertEq(d, uint64(s.t2 - 1) + G);
        vm.warp(s.t2 + 1 hours);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.refund(s, ALICE_S);
        vm.warp(d - 1);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.refund(s, ALICE_S);
        vm.prank(bob);
        c.claim(s, BOB_S, bob); // the reveal still lands
        assertEq(bob.balance, 1 ether);
    }

    function test_UnrevealedClaimIntentExpiresToRefund() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        ack(s, id, sink);
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        uint64 d = deadline(id);
        vm.warp(d);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.claim(s, BOB_S, bob);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(alice.balance, 100 ether); // value and premium back: the claimer failed
        assertEq(sink.balance, 0);
        assertEq(stage(id), 4);
    }

    function test_ClaimIntentOnlyOnceAndOnlyByClaimerSignature() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(0xBEEF, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimIntent(s, v, r, ss);
        (v, r, ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimIntent(s, 29, r, ss);
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimIntent(s, v == 27 ? 28 : 27, r,
            bytes32(0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141 - uint256(ss)));
        // a claim signature is not an intent signature
        (uint8 v2, bytes32 r2, bytes32 ss2) = signClaim(id, bob, address(0), 0);
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimIntent(s, v2, r2, ss2);
        c.claimIntent(s, v, r, ss);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector); // only once: no extending the freeze
        c.claimIntent(s, v, r, ss);
    }

    function test_IntentSignatureNotReplayableAcrossSwapsOrDeployments() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        (XmrSwapV3.Swap memory s2,) = lockEthP(0, bytes32(uint256(9)));
        vm.warp(s.t1);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimIntent(s2, v, r, ss);
        XmrSwapV3 c2 = new XmrSwapV3();
        assertTrue(c2.intentDigest(id) != c.intentDigest(id));
        assertTrue(c.intentDigest(id) != c.ackDigest(id, address(0)));
        c.claimIntent(s, v, r, ss);
        bytes32 before = c.domainSeparator();
        vm.chainId(999);
        assertTrue(c.domainSeparator() != before);
    }

    // ------------------------------------------------------------ two-step cancel
    function test_CancelIntentThenRefund() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector); // no one-step early refund any more
        c.refund(s, ALICE_S);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.CancelIntent(id, uint64(T0) + G);
        vm.prank(alice);
        c.cancelIntent(s);
        assertEq(stage(id), 6);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.Refunded(id, ALICE_S);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(alice.balance, 100 ether);
        assertEq(stage(id), 4);
    }

    function test_CancelIntentRules() public {
        (XmrSwapV3.Swap memory s,) = lockEth();
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.NotAllowed.selector);
        c.cancelIntent(s);
        uint256 snap = vm.snapshotState();
        vm.warp(s.t1);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.cancelIntent(s);
        vm.revertToState(snap);
        vm.prank(alice);
        c.setReady(s);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector); // READY: she already accepted
        c.cancelIntent(s);
        (XmrSwapV3.Swap memory s2,) = lockEthP(0, bytes32(uint256(2)));
        vm.startPrank(alice);
        c.cancelIntent(s2);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector); // only once
        c.cancelIntent(s2);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector); // and no setReady after it
        c.setReady(s2);
        vm.stopPrank();
    }

    /// A cancel intent freezes the claimer; if the owner never refunds, the swap
    /// falls back to the claimer's window (t1 <= now < t2) once the freeze ends.
    function test_CancelIntentFreezesClaimThenExpires() public {
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(alice, bob, BOB_K, ALICE_K, uint64(T0) + D1,
            uint64(T0) + D1 + 10 hours, 30 minutes, address(0), 1 ether, 0, bytes32(uint256(3)));
        vm.prank(alice);
        bytes32 id = c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, 10 hours, 30 minutes, address(0), 1 ether, 0, bytes32(uint256(3)));
        vm.warp(s.t1 - 10 minutes);
        vm.prank(alice);
        c.cancelIntent(s);
        uint64 d = deadline(id); // t1 + 20 min
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.warp(s.t1); // Bob's window would have opened, but the cancel freezes him
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.claimIntent(s, v, r, ss);
        vm.warp(d - 1);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.claimIntent(s, v, r, ss);
        vm.warp(d); // freeze over and the owner did not refund: claimer's window, owner locked out
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.refund(s, ALICE_S);
        c.claimIntent(s, v, r, ss);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
        assertEq(bob.balance, 1 ether);
    }

    function test_ExpiredCancelBeforeT1FreezesBothUntilT1() public {
        // short grace: cancel at T0, deadline T0+30min < t1 = T0+1h
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(alice, bob, BOB_K, ALICE_K, uint64(T0) + D1,
            uint64(T0) + D1 + D2, 30 minutes, address(0), 1 ether, 0, bytes32(uint256(4)));
        vm.prank(alice);
        bytes32 id = c.lock{value: 1 ether}(bob, BOB_K, ALICE_K, D1, D2, 30 minutes, address(0), 1 ether, 0, bytes32(uint256(4)));
        vm.prank(alice);
        c.cancelIntent(s);
        vm.warp(T0 + 45 minutes);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.refund(s, ALICE_S);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.claimIntent(s, v, r, ss);
        vm.warp(s.t2); // nobody claimed: a late refund is always possible
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(alice.balance, 100 ether);
    }

    function test_LateRefundFromPendingAndReady() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        vm.warp(s.t2 - 1);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.refund(s, ALICE_S);
        vm.warp(s.t2);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.WrongTime.selector); // claimer's window is over
        c.claimIntent(s, v, r, ss);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(stage(id), 4);
        (XmrSwapV3.Swap memory s2,) = lockEthP(0, bytes32(uint256(2)));
        vm.warp(s2.t2 + 1000 days);
        vm.prank(alice);
        c.refund(s2, ALICE_S);
        assertEq(alice.balance, 100 ether);
    }

    // ------------------------------------------------------------ exclusivity (fuzz)
    /// At every moment at most one party has a value-moving action, whatever intents
    /// were made and when. Value-moving: the claimer's claimIntent/claim, the owner's refund.
    function testFuzz_ExclusiveWindows(uint256 dtA, uint256 dtB, uint8 path, bool ready) public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        if (ready) { vm.prank(alice); c.setReady(s); }
        vm.warp(T0 + bound(dtA, 0, D1 + D2 + 1 days));
        path = path % 3; // 0: nothing, 1: try a cancel intent, 2: try a claim intent
        if (path == 1) { vm.prank(alice); (bool ok,) = address(c).call(abi.encodeCall(XmrSwapV3.cancelIntent, (s))); ok; }
        if (path == 2) {
            (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
            (bool ok,) = address(c).call(abi.encodeCall(XmrSwapV3.claimIntent, (s, v, r, ss))); ok;
        }
        vm.warp(block.timestamp + bound(dtB, 0, G + D1 + D2 + 1 days));
        uint256 snap = vm.snapshotState();
        bool claimerOk;
        if (stage(id) == 5) {
            vm.prank(bob);
            (claimerOk,) = address(c).call(abi.encodeCall(XmrSwapV3.claim, (s, BOB_S, bob)));
        } else {
            (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
            (claimerOk,) = address(c).call(abi.encodeCall(XmrSwapV3.claimIntent, (s, v, r, ss)));
        }
        vm.revertToState(snap);
        vm.prank(alice);
        (bool ownerOk,) = address(c).call(abi.encodeCall(XmrSwapV3.refund, (s, ALICE_S)));
        assertTrue(!(claimerOk && ownerOk), "both parties could act");
    }

    /// After a claim intent at any time, the owner cannot refund before the deadline,
    /// and the claimer's reveal works at every second before it.
    function testFuzz_ClaimIntentFreeze(uint256 dtIntent, uint256 dtProbe, bool ready) public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        if (ready) { vm.prank(alice); c.setReady(s); }
        vm.warp(T0 + bound(dtIntent, ready ? 0 : D1, D1 + D2 - 1));
        uint256 tIntent = block.timestamp;
        intent(s, id);
        uint64 d = deadline(id);
        assertEq(d, tIntent + G > s.t2 ? tIntent + G : s.t2);
        vm.warp(tIntent + bound(dtProbe, 0, d - tIntent - 1));
        uint256 snap = vm.snapshotState();
        vm.prank(alice);
        (bool refundOk,) = address(c).call(abi.encodeCall(XmrSwapV3.refund, (s, ALICE_S)));
        assertTrue(!refundOk, "refund during claim freeze");
        vm.revertToState(snap);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
    }

    /// After a cancel intent, the claimer cannot act before the deadline, and the
    /// owner's refund works at every second before it.
    function testFuzz_CancelIntentFreeze(uint256 dtIntent, uint256 dtProbe) public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.warp(T0 + bound(dtIntent, 0, D1 - 1));
        uint256 tIntent = block.timestamp;
        vm.prank(alice);
        c.cancelIntent(s);
        uint64 d = deadline(id);
        assertEq(d, tIntent + G);
        vm.warp(tIntent + bound(dtProbe, 0, G - 1));
        uint256 snap = vm.snapshotState();
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
        (bool ok,) = address(c).call(abi.encodeCall(XmrSwapV3.claimIntent, (s, v, r, ss)));
        assertTrue(!ok, "claim intent during cancel freeze");
        vm.prank(bob);
        (ok,) = address(c).call(abi.encodeCall(XmrSwapV3.claim, (s, BOB_S, bob)));
        assertTrue(!ok, "claim during cancel freeze");
        vm.revertToState(snap);
        vm.prank(alice);
        c.refund(s, ALICE_S);
    }

    // ------------------------------------------------------------ authorization & secrets
    function test_AuthAndSecrets() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.expectRevert(XmrSwapV3.NotAllowed.selector);
        c.setReady(s);
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.NotAllowed.selector);
        c.claim(s, BOB_S, alice);
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.NotAllowed.selector);
        c.refund(s, ALICE_S);
        vm.startPrank(bob);
        vm.expectRevert(XmrSwapV3.BadSecret.selector);
        c.claim(s, ALICE_S, bob);
        vm.expectRevert(XmrSwapV3.BadSecret.selector);
        c.claim(s, 0, bob);
        vm.expectRevert(XmrSwapV3.BadSecret.selector);
        c.claim(s, BOB_S + 0x1000000000000000000000000000000014def9dea2f79cd65812631a5cf5d3ed, bob); // s + l
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.claim(s, BOB_S, address(0));
        c.claim(s, BOB_S, bob);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        c.claim(s, BOB_S, bob);
        vm.stopPrank();
        vm.warp(s.t2 + G);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        c.refund(s, ALICE_S);
        assertEq(stage(id), 3);
    }

    function test_BadRefundSecretRejected() public {
        (XmrSwapV3.Swap memory s,) = lockEth();
        vm.startPrank(alice);
        c.cancelIntent(s);
        vm.expectRevert(XmrSwapV3.BadSecret.selector);
        c.refund(s, BOB_S);
        vm.stopPrank();
    }

    function test_TamperedSwapStructRejected() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.warp(s.t1);
        intent(s, id);
        s.value = 2 ether; // attacker-supplied params hash to an unknown id
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        c.claim(s, BOB_S, bob);
        s.value = 1 ether;
        s.grace = G + 1; // grace is bound into the id too
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        c.claim(s, BOB_S, bob);
    }

    // ------------------------------------------------------------ relayed claim (gasless for Bob)
    function test_RelayedClaimPaysFee() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        address to = address(0xF2E5);
        (uint8 v, bytes32 r, bytes32 ss) = signClaim(id, to, address(0), 0.001 ether);
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, to, address(0), 0.001 ether, v, r, ss);
        assertEq(to.balance, 0.999 ether);
        assertEq(relayer.balance, 0.001 ether);
        assertEq(bob.balance, 0);
    }

    function test_RelayedClaimBoundRelayer() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        (uint8 v, bytes32 r, bytes32 ss) = signClaim(id, bob, relayer, 1e15);
        vm.prank(address(0xBAD));
        vm.expectRevert(XmrSwapV3.NotAllowed.selector);
        c.claimRelayed(s, BOB_S, bob, relayer, 1e15, v, r, ss);
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, bob, relayer, 1e15, v, r, ss);
        assertEq(relayer.balance, 1e15);
    }

    function test_RelayedClaimTamperingRejected() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        (uint8 v, bytes32 r, bytes32 ss) = signClaim(id, bob, address(0), 1e15);
        vm.startPrank(relayer);
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, relayer, address(0), 1e15, v, r, ss);      // redirect funds
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 2e15, v, r, ss);          // raise fee
        vm.expectRevert(XmrSwapV3.BadSignature.selector);                  // malleated (s -> n - s)
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, v == 27 ? 28 : 27, r,
            bytes32(0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141 - uint256(ss)));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, 29, r, ss);
        (v, r, ss) = vm.sign(0xBEEF, c.claimDigest(id, bob, address(0), 1e15));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, v, r, ss);
        (v, r, ss) = signClaim(id, bob, address(0), 2 ether);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 2 ether, v, r, ss);
        // an intent signature is not a claim signature
        (v, r, ss) = vm.sign(bobPk, c.intentDigest(id));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.claimRelayed(s, BOB_S, bob, address(0), 0, v, r, ss);
        vm.stopPrank();
    }

    // ------------------------------------------------------------ premium + ack
    function test_AckRules() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.ackDigest(id, sink));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.ackLocked(s, address(0xBAD), v, r, ss);                    // signed for another address
        (uint8 v2, bytes32 r2, bytes32 ss2) = vm.sign(0xBEEF, c.ackDigest(id, sink));
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.ackLocked(s, sink, v2, r2, ss2);                           // not the claimer
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        c.ackLocked(s, sink, 29, r, ss);
        (v2, r2, ss2) = vm.sign(bobPk, c.ackDigest(id, address(0)));
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.ackLocked(s, address(0), v2, r2, ss2);
        uint256 snap = vm.snapshotState();
        vm.warp(s.t1);
        vm.expectRevert(XmrSwapV3.WrongTime.selector);
        c.ackLocked(s, sink, v, r, ss);
        vm.revertToState(snap);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.Acked(id, sink);
        c.ackLocked(s, sink, v, r, ss);
        assertEq(premiumTo(id), sink);
        (v2, r2, ss2) = vm.sign(bobPk, c.ackDigest(id, address(0xD00D)));
        vm.expectRevert(XmrSwapV3.Exists.selector);
        c.ackLocked(s, address(0xD00D), v2, r2, ss2);
    }

    function test_AckNeedsPremiumAndPending() public {
        (XmrSwapV3.Swap memory s0, bytes32 id0) = lockEth(); // premium 0
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.ackDigest(id0, sink));
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.ackLocked(s0, sink, v, r, ss);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(2)));
        (v, r, ss) = vm.sign(bobPk, c.ackDigest(id, sink));
        vm.prank(alice);
        c.cancelIntent(s); // the owner cancelled first (e.g. front-ran the ack): no ack possible
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        c.ackLocked(s, sink, v, r, ss);
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(sink.balance, 0); // premium back to the owner
    }

    function test_CancelAfterAckForfeitsPremium() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        assertEq(address(c).balance, 1 ether + P);
        ack(s, id, sink);
        vm.startPrank(alice);
        c.cancelIntent(s);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.PremiumPaid(id, sink, P);
        c.refund(s, ALICE_S);
        vm.stopPrank();
        assertEq(alice.balance, 100 ether - P);
        assertEq(sink.balance, P);
        assertEq(address(c).balance, 0);
    }

    function test_CancelAfterAckForfeitsEvenWhenRefundedAfterT2() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        ack(s, id, sink);
        vm.prank(alice);
        c.cancelIntent(s);
        vm.warp(T0 + G + 1 days); // waiting out the freeze does not dodge the premium
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(sink.balance, P);
    }

    function test_CancelWithoutAckReturnsPremium() public {
        (XmrSwapV3.Swap memory s,) = lockEthP(P, bytes32(uint256(1)));
        vm.startPrank(alice);
        c.cancelIntent(s);
        c.refund(s, ALICE_S);
        vm.stopPrank();
        assertEq(alice.balance, 100 ether);
    }

    function test_ClaimReturnsPremiumEvenAfterAck() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        ack(s, id, sink);
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        vm.expectEmit(true, false, false, true);
        emit XmrSwapV3.PremiumPaid(id, alice, P);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
        assertEq(alice.balance, 100 ether - 1 ether);
        assertEq(sink.balance, 0);
        assertEq(address(c).balance, 0);
    }

    function test_LateRefundAfterAckReturnsPremium() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        ack(s, id, sink);
        vm.warp(s.t2); // the claimer acked but never claimed
        vm.prank(alice);
        c.refund(s, ALICE_S);
        assertEq(alice.balance, 100 ether);
        assertEq(sink.balance, 0);
    }

    function test_ForfeitToRejectingContractIsDeferred() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        address rej = address(new RejectEth());
        ack(s, id, rej);
        vm.startPrank(alice);
        c.cancelIntent(s);
        c.refund(s, ALICE_S); // must not revert
        vm.stopPrank();
        assertEq(c.owed(address(0), rej), P);
        vm.prank(rej);
        c.withdraw(address(0), sink);
        assertEq(sink.balance, P);
    }

    function test_PremiumBackToRejectingOwnerIsDeferred() public {
        RejectingOwner o = new RejectingOwner(c);
        vm.deal(address(o), 2 ether);
        XmrSwapV3.Swap memory s = o.lockIt(bob, BOB_K, ALICE_K, P);
        vm.warp(s.t1);
        bytes32 id = c.swapId(s);
        intent(s, id);
        vm.prank(bob);
        c.claim(s, BOB_S, bob); // premium push to the owner fails -> owed; the claim still succeeds
        assertEq(bob.balance, 1 ether);
        assertEq(c.owed(address(0), address(o)), P);
    }

    function test_ReentrancyFromPremiumRecipientBlocked() public {
        (XmrSwapV3.Swap memory s2,) = lockEthP(P, bytes32(uint256(2)));
        vm.prank(alice);
        c.cancelIntent(s2);
        PremiumReenter pr = new PremiumReenter(c, s2);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(P, bytes32(uint256(1)));
        ack(s, id, address(pr));
        vm.startPrank(alice);
        c.cancelIntent(s);
        c.refund(s, ALICE_S);
        vm.stopPrank();
        assertTrue(pr.attempted());
        assertEq(pr.err(), XmrSwapV3.Reentrancy.selector);
        assertEq(address(pr).balance, P);
        assertEq(stage(c.swapId(s2)), 6); // still CANCELLING
    }

    /// Whatever happens and whenever, the premium goes to the acked address exactly
    /// when the refund settles a cancel intent made after an ack; otherwise to the owner.
    function testFuzz_PremiumRouting(uint256 dtAck, uint256 dtCancel, uint256 dtAct, bool doAck, bool doCancel, bool ready, bool tryClaim, uint96 prem) public {
        uint256 p = bound(uint256(prem), 1, 50 ether);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEthP(p, bytes32(uint256(77)));
        vm.warp(T0 + bound(dtAck, 0, D1 + D2));
        bool acked;
        if (doAck) {
            (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.ackDigest(id, sink));
            (acked,) = address(c).call(abi.encodeCall(XmrSwapV3.ackLocked, (s, sink, v, r, ss)));
            assertEq(acked, block.timestamp < s.t1);
        }
        if (ready) { vm.prank(alice); (bool ok,) = address(c).call(abi.encodeCall(XmrSwapV3.setReady, (s))); ok; }
        vm.warp(block.timestamp + bound(dtCancel, 0, D1));
        bool cancelled;
        if (doCancel) { vm.prank(alice); (cancelled,) = address(c).call(abi.encodeCall(XmrSwapV3.cancelIntent, (s))); }
        vm.warp(block.timestamp + bound(dtAct, 0, G + D1 + D2 + 1 days));
        uint256 a0 = alice.balance;
        if (tryClaim) {
            (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.intentDigest(id));
            (bool iok,) = address(c).call(abi.encodeCall(XmrSwapV3.claimIntent, (s, v, r, ss)));
            iok;
            vm.prank(bob);
            (bool ok,) = address(c).call(abi.encodeCall(XmrSwapV3.claim, (s, BOB_S, bob)));
            if (ok) { assertEq(alice.balance - a0, p); assertEq(sink.balance, 0); return; }
            vm.warp(block.timestamp + G + D2); // an unrevealed intent expires
        }
        vm.prank(alice);
        (bool rok,) = address(c).call(abi.encodeCall(XmrSwapV3.refund, (s, ALICE_S)));
        if (!rok) { assertEq(address(c).balance, 1 ether + p); return; }
        if (cancelled && acked) {
            assertEq(sink.balance, p);
            assertEq(alice.balance - a0, 1 ether);
        } else {
            assertEq(sink.balance, 0);
            assertEq(alice.balance - a0, 1 ether + p);
        }
        assertEq(address(c).balance, 0);
    }

    // ------------------------------------------------------------ pull-payment fallback
    function test_RejectingRecipientDefersPayout() public {
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth();
        address rej = address(new RejectEth());
        vm.warp(s.t1);
        intent(s, id);
        vm.prank(bob);
        c.claim(s, BOB_S, rej); // must not revert. (A reverted claim would still put the secret on-chain:
                                // calldata is public once broadcast, so a revert keeps nothing private.)
        assertEq(stage(id), 3);
        assertEq(c.owed(address(0), rej), 1 ether);
        vm.prank(rej);
        c.withdraw(address(0), bob);
        assertEq(bob.balance, 1 ether);
        vm.prank(rej);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.withdraw(address(0), bob);
    }

    // ------------------------------------------------------------ ERC-20
    function lockToken(address tok, uint256 amt, uint256 premium) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        Token(tok).mint(alice, amt);
        vm.prank(alice);
        Token(tok).approve(address(c), amt);
        s = _swap(tok, amt, premium, bytes32(uint256(7)));
        vm.prank(alice);
        id = c.lock{value: premium}(bob, BOB_K, ALICE_K, D1, D2, G, tok, amt, premium, bytes32(uint256(7)));
        assertEq(id, c.swapId(s));
    }

    function cancelAndRefund(XmrSwapV3.Swap memory s) internal {
        vm.startPrank(alice);
        c.cancelIntent(s);
        c.refund(s, ALICE_S);
        vm.stopPrank();
    }

    function test_Erc20HappyAndNoReturnToken() public {
        Token t = new Token();
        t.setNoReturn(true);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockToken(address(t), 1000e6, 0);
        vm.prank(alice);
        c.setReady(s);
        intent(s, id);
        (uint8 v, bytes32 r, bytes32 ss) = signClaim(id, bob, address(0), 1e6);
        vm.prank(relayer);
        c.claimRelayed(s, BOB_S, bob, address(0), 1e6, v, r, ss);
        assertEq(t.balanceOf(bob), 999e6);
        assertEq(t.balanceOf(relayer), 1e6);
    }

    function test_Erc20FeeOnTransferRejected() public {
        Token t = new Token();
        t.setFee(100);
        t.mint(alice, 1000);
        vm.prank(alice);
        t.approve(address(c), 1000);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, G, address(t), 1000, 0, 0);
    }

    function test_Erc20WrongNativeValueRejected() public {
        Token t = new Token();
        t.mint(alice, 1000);
        vm.prank(alice);
        t.approve(address(c), 1000);
        vm.startPrank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock{value: 1}(bob, BOB_K, ALICE_K, D1, D2, G, address(t), 1000, 0, 0);  // ETH sent with a token
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, G, address(t), 1000, P, 0);           // premium not sent
        vm.stopPrank();
    }

    function test_Erc20NoCodeAssetRejected() public {
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, G, address(0xdead), 1000, 0, 0);
    }

    function test_Erc20BlacklistedRecipientDefersThenWithdrawElsewhere() public {
        Token t = new Token();
        (XmrSwapV3.Swap memory s, bytes32 id) = lockToken(address(t), 500, 0);
        t.setBlocked(bob, true);
        vm.warp(s.t1);
        intent(s, id);
        vm.prank(bob);
        c.claim(s, BOB_S, bob);
        assertEq(stage(id), 3);
        assertEq(c.owed(address(t), bob), 500);
        vm.prank(bob);
        c.withdraw(address(t), address(0xC0FFEE));
        assertEq(t.balanceOf(address(0xC0FFEE)), 500);
    }

    function test_Erc20FalseReturnDefers() public {
        FalseToken t = new FalseToken();
        (XmrSwapV3.Swap memory s,) = lockToken(address(t), 500, 0);
        t.setFail(true);
        cancelAndRefund(s);
        assertEq(c.owed(address(t), alice), 500);
    }

    function test_Erc20ReturnBombHandled() public {
        BombToken t = new BombToken();
        (XmrSwapV3.Swap memory s,) = lockToken(address(t), 500, 0);
        cancelAndRefund(s);
        assertEq(t.balanceOf(alice), 500);
    }

    function test_Erc20UsdtStyleNoBoolLockClaimRefundWithPremium() public {
        UsdtLike t = new UsdtLike();
        t.mint(alice, 2000);
        vm.startPrank(alice);
        t.approve(address(c), 2000);
        c.lock{value: P}(bob, BOB_K, ALICE_K, D1, D2, G, address(t), 1000, P, bytes32(uint256(31)));
        c.lock{value: P}(bob, BOB_K, ALICE_K, D1, D2, G, address(t), 1000, P, bytes32(uint256(32)));
        vm.stopPrank();
        assertEq(t.balanceOf(address(c)), 2000);
        XmrSwapV3.Swap memory s1 = _swap(address(t), 1000, P, bytes32(uint256(31)));
        XmrSwapV3.Swap memory s2 = _swap(address(t), 1000, P, bytes32(uint256(32)));
        ack(s1, c.swapId(s1), sink);
        cancelAndRefund(s1); // cancel pays out with a no-bool transfer; premium to the acked address
        assertEq(t.balanceOf(alice), 1000);
        assertEq(sink.balance, P);
        vm.warp(s2.t1);
        intent(s2, c.swapId(s2));
        vm.prank(bob);
        c.claim(s2, BOB_S, bob);
        assertEq(t.balanceOf(bob), 1000);
        assertEq(t.balanceOf(address(c)), 0);
        assertEq(address(c).balance, 0);
        assertEq(c.owed(address(t), bob), 0);
    }

    function test_ReentrancyFromTokenBlocked() public {
        ReentrantToken t = new ReentrantToken();
        (XmrSwapV3.Swap memory s,) = lockToken(address(t), 500, 0);
        t.mint(alice, 500);
        vm.prank(alice);
        t.approve(address(c), 500);
        XmrSwapV3.Swap memory s2 = _swap(address(t), 500, 0, bytes32(uint256(8)));
        vm.prank(alice);
        c.lock(bob, BOB_K, ALICE_K, D1, D2, G, address(t), 500, 0, bytes32(uint256(8)));
        vm.prank(alice);
        c.cancelIntent(s2);
        t.arm(address(c), abi.encodeCall(XmrSwapV3.refund, (s2, ALICE_S)));
        cancelAndRefund(s);
        assertTrue(t.reentered());
        assertTrue(!t.reentrySucceeded());
        assertEq(stage(c.swapId(s2)), 6);
        assertEq(t.balanceOf(alice), 500);
        assertEq(t.balanceOf(address(c)), 500);
    }

    function test_ReentrancyFromOwnerContractBlocked() public {
        ReentrantOwnerV3 o = new ReentrantOwnerV3(c);
        vm.deal(address(o), 2 ether);
        (XmrSwapV3.Swap memory s1, XmrSwapV3.Swap memory s2) = o.setup(BOB_K, ALICE_K, bob);
        o.go(s1, s2, ALICE_S);
        assertTrue(o.attempted());
        assertEq(o.reentryError(), XmrSwapV3.Reentrancy.selector);
        assertEq(address(c).balance, 1 ether);
    }

    function test_SetReadyReentrancyFromOwnerContractBlocked() public {
        ReentrantOwnerV3 o = new ReentrantOwnerV3(c);
        o.useSetReady();
        vm.deal(address(o), 2 ether);
        (XmrSwapV3.Swap memory s1, XmrSwapV3.Swap memory s2) = o.setup(BOB_K, ALICE_K, bob);
        o.go(s1, s2, ALICE_S);
        assertTrue(o.attempted());
        assertEq(o.reentryError(), XmrSwapV3.Reentrancy.selector);
    }

    // ------------------------------------------------------------ gas snapshots (see README)
    function test_GasProfile() public {
        uint256 g;
        XmrSwapV3.Swap memory s = _swap(address(0), 1 ether, P, bytes32(uint256(1)));
        vm.prank(alice);
        g = gasleft();
        c.lock{value: 1 ether + P}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), 1 ether, P, bytes32(uint256(1)));
        emit log_named_uint("lock (ETH + premium)", g - gasleft());
        bytes32 id = c.swapId(s);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, c.ackDigest(id, sink));
        g = gasleft();
        c.ackLocked(s, sink, v, r, ss);
        emit log_named_uint("ackLocked", g - gasleft());
        vm.prank(alice);
        g = gasleft();
        c.setReady(s);
        emit log_named_uint("setReady", g - gasleft());
        (v, r, ss) = vm.sign(bobPk, c.intentDigest(id));
        g = gasleft();
        c.claimIntent(s, v, r, ss);
        emit log_named_uint("claimIntent", g - gasleft());
        (v, r, ss) = signClaim(id, bob, address(0), 1e15);
        vm.prank(relayer);
        g = gasleft();
        c.claimRelayed(s, BOB_S, bob, address(0), 1e15, v, r, ss);
        emit log_named_uint("claimRelayed (ETH, premium back)", g - gasleft());
        (XmrSwapV3.Swap memory s2,) = lockEthP(0, bytes32(uint256(2)));
        vm.prank(alice);
        g = gasleft();
        c.cancelIntent(s2);
        emit log_named_uint("cancelIntent", g - gasleft());
        vm.prank(alice);
        g = gasleft();
        c.refund(s2, ALICE_S);
        emit log_named_uint("refund (ETH)", g - gasleft());
    }
}

contract ReentrantOwnerV3 {
    XmrSwapV3 c;
    XmrSwapV3.Swap other;
    uint256 sec;
    bool public attempted;
    bool public trySetReady; // re-enter with setReady instead of refund
    bytes4 public reentryError;
    constructor(XmrSwapV3 c_) { c = c_; }
    function useSetReady() external { trySetReady = true; }
    function setup(uint256 bk, uint256 ak, address bob) external
        returns (XmrSwapV3.Swap memory s1, XmrSwapV3.Swap memory s2)
    {
        uint64 t1 = uint64(block.timestamp) + 1 hours;
        c.lock{value: 1 ether}(bob, bk, ak, 1 hours, 2 hours, 6 hours, address(0), 1 ether, 0, bytes32(uint256(1)));
        c.lock{value: 1 ether}(bob, bk, ak, 1 hours, 2 hours, 6 hours, address(0), 1 ether, 0, bytes32(uint256(2)));
        s1 = XmrSwapV3.Swap(address(this), bob, bk, ak, t1, t1 + 2 hours, 6 hours, address(0), 1 ether, 0, bytes32(uint256(1)));
        s2 = XmrSwapV3.Swap(address(this), bob, bk, ak, t1, t1 + 2 hours, 6 hours, address(0), 1 ether, 0, bytes32(uint256(2)));
        c.cancelIntent(s1);
        if (!trySetReady) c.cancelIntent(s2);
    }
    function go(XmrSwapV3.Swap calldata s1, XmrSwapV3.Swap calldata s2, uint256 secret) external {
        other = s2;
        sec = secret;
        c.refund(s1, secret);
    }
    receive() external payable {
        if (attempted) return;
        attempted = true;
        if (trySetReady) {
            try c.setReady(other) {} catch (bytes memory err) { reentryError = bytes4(err); }
        } else {
            try c.refund(other, sec) {} catch (bytes memory err) { reentryError = bytes4(err); }
        }
    }
}

contract RejectingOwner {
    XmrSwapV3 c;
    constructor(XmrSwapV3 c_) { c = c_; }
    function lockIt(address bob, uint256 bk, uint256 ak, uint256 premium) external returns (XmrSwapV3.Swap memory s) {
        uint64 t1 = uint64(block.timestamp) + 1 hours;
        c.lock{value: 1 ether + premium}(bob, bk, ak, 1 hours, 2 hours, 6 hours, address(0), 1 ether, premium, bytes32(uint256(1)));
        s = XmrSwapV3.Swap(address(this), bob, bk, ak, t1, t1 + 2 hours, 6 hours, address(0), 1 ether, premium, bytes32(uint256(1)));
    }
    receive() external payable { revert("no"); }
}

contract PremiumReenter {
    XmrSwapV3 c;
    XmrSwapV3.Swap other;
    bool public attempted;
    bytes4 public err;
    constructor(XmrSwapV3 c_, XmrSwapV3.Swap memory s) { c = c_; other = s; }
    receive() external payable {
        attempted = true;
        try c.refund(other, 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25) {} catch (bytes memory e) { err = bytes4(e); }
    }
}
